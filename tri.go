package sshego

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	ssh "github.com/glycerine/sshego/xendor/github.com/glycerine/xcryptossh"
)

var ErrShutdown = fmt.Errorf("shutting down")

// Tricorder records (holds) three key objects:
//   an *ssh.Client, the underlyign net.Conn, and a
//   set of ssh.Channel(s).
//
// Tricorder supports auto reconnect when disconnected.
//
// There should be exactly one Tricorder per (username, sshdHost, sshdPort) triple.
//
type Tricorder struct {
	Name string

	// shuts down everything, include the cli
	Halt *ssh.Halter

	// shared with cfg
	ClientReconnectNeededTower *UHPTower

	// optional, parent can provide us
	// a Halter, and we will ParentHalt.AddDownstream(self.ChannelHalt)
	parentHalt *ssh.Halter

	// should only reflect close of the internal sshChannels, not cli nor nc.
	// This is not public because we may replace it internally during run.
	channelsHalt *ssh.Halter

	dc  *DialConfig
	cfg *SshegoConfig

	sshdHostPort string

	cli         *ssh.Client
	nc          io.Closer
	uhp         *UHP
	sshChannels map[net.Conn]context.CancelFunc

	getChannelCh      chan *getChannelTicket
	getCliCh          chan *ssh.Client
	getNcCh           chan io.Closer
	reconnectNeededCh chan *UHP

	tofu bool

	retries             int           // example: 10
	pauseBetweenRetries time.Duration // example: 1000 * time.Millisecond

	lastConnectTime time.Time
}

/*
NewTricorder has got to wait to allocate
ssh.Channel until requested. Otherwise we
make too many, and get them mixed up.
*/
func NewTricorder(dc *DialConfig, halt *ssh.Halter, name string) (tri *Tricorder, err error) {

	cfg, err := dc.DeriveNewConfig()
	if err != nil {
		return nil, err
	}
	sshdHostPort := fmt.Sprintf("%v:%v", dc.Sshdhost, dc.Sshdport)

	tri = &Tricorder{
		Name:         name,
		dc:           dc,
		cfg:          cfg,
		sshdHostPort: sshdHostPort,
		parentHalt:   halt,
		Halt:         ssh.NewHalter(),
		channelsHalt: ssh.NewHalter(),

		sshChannels: make(map[net.Conn]context.CancelFunc),

		reconnectNeededCh:   make(chan *UHP, 1),
		getChannelCh:        make(chan *getChannelTicket),
		getCliCh:            make(chan *ssh.Client),
		getNcCh:             make(chan io.Closer),
		tofu:                dc.TofuAddIfNotKnown,
		retries:             10,
		pauseBetweenRetries: 1000 * time.Millisecond,
	}
	tri.uhp = &UHP{
		User:     tri.dc.Mylogin,
		HostPort: tri.sshdHostPort,
		Nickname: tri.dc.DestNickname,
	}

	if tri.parentHalt != nil {
		tri.parentHalt.AddDownstream(tri.Halt)
	}
	tri.Halt.AddDownstream(tri.channelsHalt)
	cfg.ClientReconnectNeededTower.Subscribe(tri.reconnectNeededCh)
	tri.ClientReconnectNeededTower = cfg.ClientReconnectNeededTower

	err = tri.startReconnectLoop()
	if err != nil {
		return nil, err
	}
	return tri, nil
}

// CustomInprocStreamChanName is how sshego/reptile specific
// channels are named.
//const CustomInprocStreamChanName = "custom-inproc-stream"
const CustomInprocStreamChanName = "direct-tcpip"

func (t *Tricorder) closeChannels() {
	if len(t.sshChannels) > 0 {
		for ch, cancel := range t.sshChannels {
			ch.Close()
			if cancel != nil {
				cancel()
			}
		}
	}
	t.sshChannels = make(map[net.Conn]context.CancelFunc)
}

func (t *Tricorder) startReconnectLoop() error {

	// do the initial connect.
	err := t.helperNewClientConnect(context.Background())
	if err != nil {
		return err
	}

	go func() {
		defer func() {
			t.channelsHalt.RequestStop()
			t.channelsHalt.MarkDone()
			t.Halt.RequestStop()
			t.Halt.MarkDone()
			if t.parentHalt != nil {
				t.parentHalt.RemoveDownstream(t.Halt)
			}
			t.closeChannels()
		}()
		for {
			select {
			case <-t.Halt.ReqStopChan():
				return
			case uhp := <-t.reconnectNeededCh:
				pp("%s Tricorder sees reconnectNeeded to '%#v'!!", uhp, t.Name)

				if uhp.User != t.uhp.User {
					panic(fmt.Sprintf("%s yikes, bad! uhp from reconnectNeededChan asks for change of user: '%v' != '%v' previous", t.Name, uhp.User, t.uhp.User))
				}
				if uhp.HostPort != t.uhp.HostPort {
					panic(fmt.Sprintf("%s yikes, bad! uhp from reconnectNeededChan asks for change of hostport: '%v' != '%v' previous", t.Name, uhp.HostPort, t.uhp.HostPort))
				}
				now := time.Now()
				if now.Sub(t.lastConnectTime) < time.Second {
					pp("%s Tricorder ignoring reconnectNeeded within "+
						"1 second of successful connection.", t.Name)
					continue
				}
				t.uhp = uhp
				t.closeChannels()

				t.channelsHalt.RequestStop()
				t.channelsHalt.MarkDone()

				t.Halt.RemoveDownstream(t.channelsHalt)
				t.channelsHalt = ssh.NewHalter()
				t.Halt.AddDownstream(t.channelsHalt)

				t.cli = nil
				t.nc = nil
				// need to reconnect!
				ctx := context.Background()
				err := t.helperNewClientConnect(ctx)
				if err == ErrShutdown {
					return
				}
				panicOn(err)

				// provide current state
			case t.getCliCh <- t.cli:
			case t.getNcCh <- t.nc:
				pp("%s tri sent t.nc='%#v'", t.Name, t.nc)

				// bring up a new channel
			case tk := <-t.getChannelCh:
				t.helperGetChannel(tk)
			}
		}
	}()
	return nil
}

// only reconnect, don't open any new channels!
func (t *Tricorder) helperNewClientConnect(ctx context.Context) (err error) {

	pp("%s Tricorder.helperNewClientConnect starting! t.uhp='%#v'.", t.Name, t.uhp)

	defer func() {
		if err != nil {
			t.lastConnectTime = time.Now()
		}
	}()

	destHost, port, err := SplitHostPort(t.uhp.HostPort)
	_, _ = destHost, port
	if err != nil {
		return err
	}

	// TODO: pw & totpUrl currently required in the test... change this.
	//pw := t.dc.Pw
	//totpUrl := t.dc.TotpUrl

	//t.cfg.AddIfNotKnown = false
	var sshcli *ssh.Client
	tries := t.retries
	pause := t.pauseBetweenRetries
	if t.cfg.KnownHosts == nil {
		panic("problem! t.cfg.KnownHosts is nil")
	}
	if t.cfg.PrivateKeyPath == "" {
		panic("problem! t.cfg.PrivateKeyPath is empty")
	}

	var okCtx context.Context

	for i := 0; i < tries; i++ {
		pp("%s Tricorder.helperNewClientConnect() calling t.dc.Dial(), i=%v", t.Name, i)

		// check for shutdown request
		select {
		case <-t.Halt.ReqStopChan():
			return ErrShutdown
		default:
		}

		ctxChild, cancelChildCtx := context.WithCancel(ctx)

		//t.cfg.AddIfNotKnown = t.tofu
		//t.dc.TofuAddIfNotKnown = t.tofu

		_, sshcli, _, err = t.dc.Dial(ctxChild, t.cfg, true)
		if err == nil {
			t.tofu = false
			t.cfg.AddIfNotKnown = false
			okCtx = ctxChild

			if sshcli == nil {
				panic("err must not be nil if sshcli is nil, back from cfg.SSHConnect")
			}
			break
		} else {
			cancelChildCtx()
			errs := err.Error()
			if strings.Contains(errs, "Re-run without -new") {
				if t.tofu {
					p("auto-handling tofu b/c t.tofu is true")
					t.tofu = false
					t.dc.TofuAddIfNotKnown = false
					t.cfg.AddIfNotKnown = false
					continue
				}
				return err
			}
			if strings.Contains(errs, "getsockopt: connection refused") {
				pp("%s Tricorder.helperNewClientConnect: ignoring 'connection refused' and retrying after %v. connecting to '%#v'", t.Name, pause, t.uhp)
				time.Sleep(pause)
				continue
			}
			pp("%s Tricorder: err = '%v'. retrying after %v", t.Name, err, pause)
			time.Sleep(pause)
			continue
		}
	} // end i over tries

	if sshcli != nil && okCtx != nil {
		sshcli.TmpCtx = okCtx
	}
	if err != nil {
		return err
	}
	pp("good: %s Tricorder.helperNewClientConnect succeeded to '%#v'.", t.Name, t.uhp)
	t.cli = sshcli
	if t.cli != nil {
		t.nc = t.cli.NcCloser()
	} else {
		panic("why no NcCloser()???")
	}
	return nil
}

func (t *Tricorder) helperGetChannel(tk *getChannelTicket) {

	pp("%s Tricorder.helperGetChannel starting! t.uhp='%#v'", t.Name, t.uhp)

	var ch ssh.Channel
	var in <-chan *ssh.Request
	var err error
	if t.cli == nil {
		pp("%s Tricorder.helperGetChannel: saw nil cli, so making new client", t.Name)
		err = t.helperNewClientConnect(tk.ctx)
		if err != nil {
			tk.err = err
			close(tk.done)
			return
		}
	}

	pp("%s Tricorder.helperGetChannel: had cli already, so calling t.cli.Dial()", t.Name)
	discardCtx, discardCtxCancel := context.WithCancel(tk.ctx)

	if tk.typ == "direct-tcpip" {
		hp := strings.Trim(tk.targetHostPort, "\n\r\t ")

		pp("%s Tricorder.helperGetChannel dialing hp='%v'", t.Name, hp)
		ch, err = t.cli.DialWithContext(discardCtx, "tcp", hp)

	} else {

		ch, in, err = t.cli.OpenChannel(tk.ctx, tk.typ, nil, t.channelsHalt)
		if err == nil {
			go DiscardRequestsExceptKeepalives(discardCtx, in, t.channelsHalt.ReqStopChan())
		}
	}
	if ch != nil {
		t.sshChannels[ch] = discardCtxCancel

		if t.cfg.IdleTimeoutDur > 0 {
			sshChan, ok := ch.(ssh.Channel)
			if ok {
				sshChan.SetIdleTimeout(t.cfg.IdleTimeoutDur)
			}
		}
	}

	tk.sshChannel = ch
	tk.err = err

	close(tk.done)
}

type getChannelTicket struct {
	done           chan struct{}
	sshChannel     ssh.Channel
	targetHostPort string // leave empty for "custom-inproc-stream", else downstream addr
	typ            string // "direct-tcpip" or "custom-inproc-stream"
	err            error
	ctx            context.Context
}

func newGetChannelTicket(ctx context.Context) *getChannelTicket {
	return &getChannelTicket{
		done: make(chan struct{}),
		ctx:  ctx,
	}
}

// typ can be "direct-tcpip" (specify destHostPort), or "custom-inproc-stream"
// in which case leave destHostPort as the empty string.
func (t *Tricorder) SSHChannel(ctx context.Context, typ, targetHostPort string) (ssh.Channel, error) {
	tk := newGetChannelTicket(ctx)
	tk.typ = typ
	tk.targetHostPort = targetHostPort
	t.getChannelCh <- tk
	<-tk.done
	return tk.sshChannel, tk.err
}

func (t *Tricorder) Cli() (cli *ssh.Client, err error) {
	select {
	case cli = <-t.getCliCh:
	case <-t.Halt.ReqStopChan():
		err = ErrShutdown
	}
	return
}

func (t *Tricorder) Nc() (nc io.Closer, err error) {
	select {
	case nc = <-t.getNcCh:
	case <-t.Halt.ReqStopChan():
		err = ErrShutdown
	}
	return
}
