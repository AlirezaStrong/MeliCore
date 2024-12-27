package dokodemo

import (
	"context"
	"sync/atomic"

	"github.com/GFW-knocker/Xray-core/common"
	"github.com/GFW-knocker/Xray-core/common/buf"
	"github.com/GFW-knocker/Xray-core/common/errors"
	"github.com/GFW-knocker/Xray-core/common/log"
	"github.com/GFW-knocker/Xray-core/common/net"
	"github.com/GFW-knocker/Xray-core/common/protocol"
	"github.com/GFW-knocker/Xray-core/common/session"
	"github.com/GFW-knocker/Xray-core/common/signal"
	"github.com/GFW-knocker/Xray-core/common/task"
	"github.com/GFW-knocker/Xray-core/core"
	"github.com/GFW-knocker/Xray-core/features/policy"
	"github.com/GFW-knocker/Xray-core/features/routing"
	"github.com/GFW-knocker/Xray-core/transport/internet/stat"
)

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		d := new(DokodemoDoor)
		err := core.RequireFeatures(ctx, func(pm policy.Manager) error {
			return d.Init(config.(*Config), pm, session.SockoptFromContext(ctx))
		})
		return d, err
	}))
}

type DokodemoDoor struct {
	policyManager policy.Manager
	config        *Config
	address       net.Address
	port          net.Port
	sockopt       *session.Sockopt
}

// Init initializes the DokodemoDoor instance with necessary parameters.
func (d *DokodemoDoor) Init(config *Config, pm policy.Manager, sockopt *session.Sockopt) error {
	if len(config.Networks) == 0 {
		return errors.New("no network specified")
	}
	d.config = config
	d.address = config.GetPredefinedAddress()
	d.port = net.Port(config.Port)
	d.policyManager = pm
	d.sockopt = sockopt

	return nil
}

// Network implements proxy.Inbound.
func (d *DokodemoDoor) Network() []net.Network {
	return d.config.Networks
}

func (d *DokodemoDoor) policy() policy.Session {
	config := d.config
	p := d.policyManager.ForLevel(config.UserLevel)
	return p
}

type hasHandshakeAddressContext interface {
	HandshakeAddressContext(ctx context.Context) net.Address
}

// Process implements proxy.Inbound.
func (d *DokodemoDoor) Process(ctx context.Context, network net.Network, conn stat.Connection, dispatcher routing.Dispatcher) error {
	errors.LogDebug(ctx, "processing connection from: ", conn.RemoteAddr())
	dest := net.Destination{
		Network: network,
		Address: d.address,
		Port:    d.port,
	}

	destinationOverridden := false
	if d.config.FollowRedirect {
		outbounds := session.OutboundsFromContext(ctx)
		if len(outbounds) > 0 {
			ob := outbounds[len(outbounds)-1]
			if ob.Target.IsValid() {
				dest = ob.Target
				destinationOverridden = true
			}
		}
		if handshake, ok := conn.(hasHandshakeAddressContext); ok && !destinationOverridden {
			addr := handshake.HandshakeAddressContext(ctx)
			if addr != nil {
				dest.Address = addr
				destinationOverridden = true
			}
		}
	}
	if !dest.IsValid() || dest.Address == nil {
		return errors.New("unable to get destination")
	}

	inbound := session.InboundFromContext(ctx)
	inbound.Name = "dokodemo-door"
	inbound.CanSpliceCopy = 1
	inbound.User = &protocol.MemoryUser{
		Level: d.config.UserLevel,
	}

	ctx = log.ContextWithAccessMessage(ctx, &log.AccessMessage{
		From:   conn.RemoteAddr(),
		To:     dest,
		Status: log.AccessAccepted,
		Reason: "",
	})
	errors.LogInfo(ctx, "received request for ", conn.RemoteAddr())

	plcy := d.policy()
	ctx, cancel := context.WithCancel(ctx)
	timer := signal.CancelAfterInactivity(ctx, cancel, plcy.Timeouts.ConnectionIdle)

	if inbound != nil {
		inbound.Timer = timer
	}

	ctx = policy.ContextWithBufferPolicy(ctx, plcy.Buffer)
	link, err := dispatcher.Dispatch(ctx, dest)
	if err != nil {
		return errors.New("failed to dispatch request").Base(err)
	}

	requestCount := int32(1)
	requestDone := func() error {
		defer func() {
			if atomic.AddInt32(&requestCount, -1) == 0 {
				timer.SetTimeout(plcy.Timeouts.DownlinkOnly)
			}
		}()

		var reader buf.Reader
		if dest.Network == net.Network_UDP {
			reader = buf.NewPacketReader(conn)
		} else {
			reader = buf.NewReader(conn)
		}
		if err := buf.Copy(reader, link.Writer, buf.UpdateActivity(timer)); err != nil {
			return errors.New("failed to transport request").Base(err)
		}

		return nil
	}

	tproxyRequest := func() error {
		return nil
	}

	var writer buf.Writer
	if network == net.Network_TCP {
		writer = buf.NewWriter(conn)
	} else {
		// if we are in TPROXY mode, use linux's udp forging functionality
		if !destinationOverridden {
			writer = &buf.SequentialWriter{Writer: conn}
		} else {
			back := conn.RemoteAddr().(*net.UDPAddr)
			if !dest.Address.Family().IsIP() {
				if len(back.IP) == 4 {
					dest.Address = net.AnyIP
				} else {
					dest.Address = net.AnyIPv6
				}
			}
			addr := &net.UDPAddr{
				IP:   dest.Address.IP(),
				Port: int(dest.Port),
			}
			var mark int
			if d.sockopt != nil {
				mark = int(d.sockopt.Mark)
			}
			pConn, err := FakeUDP(addr, mark)
			if err != nil {
				return err
			}
			writer = NewPacketWriter(pConn, &dest, mark, back)
			/*
				sockopt := &internet.SocketConfig{
					Tproxy: internet.SocketConfig_TProxy,
				}
				if dest.Address.Family().IsIP() {
					sockopt.BindAddress = dest.Address.IP()
					sockopt.BindPort = uint32(dest.Port)
				}
				if d.sockopt != nil {
					sockopt.Mark = d.sockopt.Mark
				}
				tConn, err := internet.DialSystem(ctx, net.DestinationFromAddr(conn.RemoteAddr()), sockopt)
				if err != nil {
					return err
				}
				defer tConn.Close()

				writer = &buf.SequentialWriter{Writer: tConn}
				tReader := buf.NewPacketReader(tConn)
				requestCount++
				tproxyRequest = func() error {
					defer func() {
						if atomic.AddInt32(&requestCount, -1) == 0 {
							timer.SetTimeout(plcy.Timeouts.DownlinkOnly)
						}
					}()
					if err := buf.Copy(tReader, link.Writer, buf.UpdateActivity(timer)); err != nil {
						return errors.New("failed to transport request (TPROXY conn)").Base(err)
					}
					return nil
				}
			*/
		}
	}

	responseDone := func() error {
		defer timer.SetTimeout(plcy.Timeouts.UplinkOnly)

		if pw, ok := writer.(*PacketWriter); ok {
			defer pw.Close()
		}

		if err := buf.Copy(link.Reader, writer, buf.UpdateActivity(timer)); err != nil {
			return errors.New("failed to transport response").Base(err)
		}
		return nil
	}

	if err := task.Run(ctx, task.OnSuccess(func() error {
		return task.Run(ctx, requestDone, tproxyRequest)
	}, task.Close(link.Writer)), responseDone); err != nil {
		common.Interrupt(link.Reader)
		common.Interrupt(link.Writer)
		return errors.New("connection ends").Base(err)
	}

	return nil
}

func NewPacketWriter(conn net.PacketConn, d *net.Destination, mark int, back *net.UDPAddr) buf.Writer {
	writer := &PacketWriter{
		conn:  conn,
		conns: make(map[net.Destination]net.PacketConn),
		mark:  mark,
		back:  back,
	}
	writer.conns[*d] = conn
	return writer
}

type PacketWriter struct {
	conn  net.PacketConn
	conns map[net.Destination]net.PacketConn
	mark  int
	back  *net.UDPAddr
}

func (w *PacketWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for {
		mb2, b := buf.SplitFirst(mb)
		mb = mb2
		if b == nil {
			break
		}
		var err error
		if b.UDP != nil && b.UDP.Address.Family().IsIP() {
			conn := w.conns[*b.UDP]
			if conn == nil {
				conn, err = FakeUDP(
					&net.UDPAddr{
						IP:   b.UDP.Address.IP(),
						Port: int(b.UDP.Port),
					},
					w.mark,
				)
				if err != nil {
					errors.LogInfo(context.Background(), err.Error())
					b.Release()
					continue
				}
				w.conns[*b.UDP] = conn
			}
			_, err = conn.WriteTo(b.Bytes(), w.back)
			if err != nil {
				errors.LogInfo(context.Background(), err.Error())
				w.conns[*b.UDP] = nil
				conn.Close()
			}
			b.Release()
		} else {
			_, err = w.conn.WriteTo(b.Bytes(), w.back)
			b.Release()
			if err != nil {
				buf.ReleaseMulti(mb)
				return err
			}
		}
	}
	return nil
}

func (w *PacketWriter) Close() error {
	for _, conn := range w.conns {
		if conn != nil {
			conn.Close()
		}
	}
	return nil
}
