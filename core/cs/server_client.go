package cs

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/rand"
	"net"
	"strconv"
	"sync"

	"github.com/apernet/hysteria/core/acl"
	"github.com/apernet/hysteria/core/transport"
	"github.com/apernet/hysteria/core/utils"
	"github.com/lucas-clemente/quic-go"
	"github.com/lunixbochs/struc"
	"github.com/prometheus/client_golang/prometheus"
)

const udpBufferSize = 4096

type serverClient struct {
	CC              quic.Connection
	Transport       *transport.ServerTransport
	Auth            []byte
	DisableUDP      bool
	ACLEngine       *acl.Engine
	CTCPRequestFunc TCPRequestFunc
	CTCPErrorFunc   TCPErrorFunc
	CUDPRequestFunc UDPRequestFunc
	CUDPErrorFunc   UDPErrorFunc

	UpCounter, DownCounter prometheus.Counter
	ConnGauge              prometheus.Gauge

	udpSessionMutex  sync.RWMutex
	udpSessionMap    map[uint32]transport.STPacketConn
	nextUDPSessionID uint32
	udpDefragger     defragger
}

func newServerClient(cc quic.Connection, tr *transport.ServerTransport, auth []byte, disableUDP bool, ACLEngine *acl.Engine,
	CTCPRequestFunc TCPRequestFunc, CTCPErrorFunc TCPErrorFunc,
	CUDPRequestFunc UDPRequestFunc, CUDPErrorFunc UDPErrorFunc,
	UpCounterVec, DownCounterVec *prometheus.CounterVec,
	ConnGaugeVec *prometheus.GaugeVec,
) *serverClient {
	sc := &serverClient{
		CC:              cc,
		Transport:       tr,
		Auth:            auth,
		DisableUDP:      disableUDP,
		ACLEngine:       ACLEngine,
		CTCPRequestFunc: CTCPRequestFunc,
		CTCPErrorFunc:   CTCPErrorFunc,
		CUDPRequestFunc: CUDPRequestFunc,
		CUDPErrorFunc:   CUDPErrorFunc,
		udpSessionMap:   make(map[uint32]transport.STPacketConn),
	}
	if UpCounterVec != nil && DownCounterVec != nil && ConnGaugeVec != nil {
		authB64 := base64.StdEncoding.EncodeToString(auth)
		sc.UpCounter = UpCounterVec.WithLabelValues(authB64)
		sc.DownCounter = DownCounterVec.WithLabelValues(authB64)
		sc.ConnGauge = ConnGaugeVec.WithLabelValues(authB64)
	}
	return sc
}

func (c *serverClient) ClientAddr() net.Addr {
	// quic.Connection's remote address may change since we have connection migration now,
	// so logs need to dynamically get the remote address every time.
	return c.CC.RemoteAddr()
}

func (c *serverClient) Run() error {
	if !c.DisableUDP {
		go func() {
			for {
				msg, err := c.CC.ReceiveMessage()
				if err != nil {
					break
				}
				c.handleMessage(msg)
			}
		}()
	}
	for {
		stream, err := c.CC.AcceptStream(context.Background())
		if err != nil {
			return err
		}
		if c.ConnGauge != nil {
			c.ConnGauge.Inc()
		}
		go func() {
			stream := &qStream{stream}
			c.handleStream(stream)
			_ = stream.Close()
			if c.ConnGauge != nil {
				c.ConnGauge.Dec()
			}
		}()
	}
}

func (c *serverClient) handleStream(stream quic.Stream) {
	// Read request
	var req clientRequest
	err := struc.Unpack(stream, &req)
	if err != nil {
		return
	}
	if !req.UDP {
		// TCP connection
		c.handleTCP(stream, req.Host, req.Port)
	} else if !c.DisableUDP {
		// UDP connection
		c.handleUDP(stream)
	} else {
		// UDP disabled
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "UDP disabled",
		})
	}
}

func (c *serverClient) handleMessage(msg []byte) {
	var udpMsg udpMessage
	err := struc.Unpack(bytes.NewBuffer(msg), &udpMsg)
	if err != nil {
		return
	}
	dfMsg := c.udpDefragger.Feed(udpMsg)
	if dfMsg == nil {
		return
	}
	c.udpSessionMutex.RLock()
	conn, ok := c.udpSessionMap[dfMsg.SessionID]
	c.udpSessionMutex.RUnlock()
	if ok {
		// Session found, send the message
		action, arg := acl.ActionDirect, ""
		var isDomain bool
		var ipAddr *net.IPAddr
		var err error
		if c.ACLEngine != nil {
			action, arg, isDomain, ipAddr, err = c.ACLEngine.ResolveAndMatch(dfMsg.Host, dfMsg.Port, true)
		} else {
			ipAddr, isDomain, err = c.Transport.ResolveIPAddr(dfMsg.Host)
		}
		if err != nil && !(isDomain && c.Transport.ProxyEnabled()) { // Special case for domain requests + SOCKS5 outbound
			return
		}
		switch action {
		case acl.ActionDirect, acl.ActionProxy: // Treat proxy as direct on server side
			addrEx := &transport.AddrEx{
				IPAddr: ipAddr,
				Port:   int(dfMsg.Port),
			}
			if isDomain {
				addrEx.Domain = dfMsg.Host
			}
			_, _ = conn.WriteTo(dfMsg.Data, addrEx)
			if c.UpCounter != nil {
				c.UpCounter.Add(float64(len(dfMsg.Data)))
			}
		case acl.ActionBlock:
			// Do nothing
		case acl.ActionHijack:
			hijackIPAddr, isDomain, err := c.Transport.ResolveIPAddr(arg)
			if err == nil || (isDomain && c.Transport.ProxyEnabled()) { // Special case for domain requests + SOCKS5 outbound
				addrEx := &transport.AddrEx{
					IPAddr: hijackIPAddr,
					Port:   int(dfMsg.Port),
				}
				if isDomain {
					addrEx.Domain = arg
				}
				_, _ = conn.WriteTo(dfMsg.Data, addrEx)
				if c.UpCounter != nil {
					c.UpCounter.Add(float64(len(dfMsg.Data)))
				}
			}
		default:
			// Do nothing
		}
	}
}

func (c *serverClient) handleTCP(stream quic.Stream, host string, port uint16) {
	addrStr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	action, arg := acl.ActionDirect, ""
	var isDomain bool
	var ipAddr *net.IPAddr
	var err error
	if c.ACLEngine != nil {
		action, arg, isDomain, ipAddr, err = c.ACLEngine.ResolveAndMatch(host, port, false)
	} else {
		ipAddr, isDomain, err = c.Transport.ResolveIPAddr(host)
	}
	if err != nil && !(isDomain && c.Transport.ProxyEnabled()) { // Special case for domain requests + SOCKS5 outbound
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "host resolution failure",
		})
		c.CTCPErrorFunc(c.ClientAddr(), c.Auth, addrStr, err)
		return
	}
	c.CTCPRequestFunc(c.ClientAddr(), c.Auth, addrStr, action, arg)

	var conn net.Conn // Connection to be piped
	switch action {
	case acl.ActionDirect, acl.ActionProxy: // Treat proxy as direct on server side
		addrEx := &transport.AddrEx{
			IPAddr: ipAddr,
			Port:   int(port),
		}
		if isDomain {
			addrEx.Domain = host
		}
		conn, err = c.Transport.DialTCP(addrEx)
		if err != nil {
			_ = struc.Pack(stream, &serverResponse{
				OK:      false,
				Message: err.Error(),
			})
			c.CTCPErrorFunc(c.ClientAddr(), c.Auth, addrStr, err)
			return
		}
	case acl.ActionBlock:
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "blocked by ACL",
		})
		return
	case acl.ActionHijack:
		hijackIPAddr, isDomain, err := c.Transport.ResolveIPAddr(arg)
		if err != nil && !(isDomain && c.Transport.ProxyEnabled()) { // Special case for domain requests + SOCKS5 outbound
			_ = struc.Pack(stream, &serverResponse{
				OK:      false,
				Message: err.Error(),
			})
			c.CTCPErrorFunc(c.ClientAddr(), c.Auth, addrStr, err)
			return
		}
		addrEx := &transport.AddrEx{
			IPAddr: hijackIPAddr,
			Port:   int(port),
		}
		if isDomain {
			addrEx.Domain = arg
		}
		conn, err = c.Transport.DialTCP(addrEx)
		if err != nil {
			_ = struc.Pack(stream, &serverResponse{
				OK:      false,
				Message: err.Error(),
			})
			c.CTCPErrorFunc(c.ClientAddr(), c.Auth, addrStr, err)
			return
		}
	default:
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "ACL error",
		})
		return
	}
	// So far so good if we reach here
	defer conn.Close()
	err = struc.Pack(stream, &serverResponse{
		OK: true,
	})
	if err != nil {
		return
	}
	if c.UpCounter != nil && c.DownCounter != nil {
		err = utils.Pipe2Way(stream, conn, func(i int) {
			if i > 0 {
				c.UpCounter.Add(float64(i))
			} else {
				c.DownCounter.Add(float64(-i))
			}
		})
	} else {
		err = utils.Pipe2Way(stream, conn, nil)
	}
	c.CTCPErrorFunc(c.ClientAddr(), c.Auth, addrStr, err)
}

func (c *serverClient) handleUDP(stream quic.Stream) {
	// Like in SOCKS5, the stream here is only used to maintain the UDP session. No need to read anything from it
	conn, err := c.Transport.ListenUDP()
	if err != nil {
		_ = struc.Pack(stream, &serverResponse{
			OK:      false,
			Message: "UDP initialization failed",
		})
		c.CUDPErrorFunc(c.ClientAddr(), c.Auth, 0, err)
		return
	}
	defer conn.Close()

	var id uint32
	c.udpSessionMutex.Lock()
	id = c.nextUDPSessionID
	c.udpSessionMap[id] = conn
	c.nextUDPSessionID += 1
	c.udpSessionMutex.Unlock()

	err = struc.Pack(stream, &serverResponse{
		OK:           true,
		UDPSessionID: id,
	})
	if err != nil {
		return
	}
	c.CUDPRequestFunc(c.ClientAddr(), c.Auth, id)

	// Receive UDP packets, send them to the client
	go func() {
		buf := make([]byte, udpBufferSize)
		for {
			n, rAddr, err := conn.ReadFrom(buf)
			if n > 0 {
				var msgBuf bytes.Buffer
				msg := udpMessage{
					SessionID: id,
					Host:      rAddr.IP.String(),
					Port:      uint16(rAddr.Port),
					FragCount: 1,
					Data:      buf[:n],
				}
				// try no frag first
				_ = struc.Pack(&msgBuf, &msg)
				sendErr := c.CC.SendMessage(msgBuf.Bytes())
				if sendErr != nil {
					if errSize, ok := sendErr.(quic.ErrMessageToLarge); ok {
						// need to frag
						msg.MsgID = uint16(rand.Intn(0xFFFF)) + 1 // msgID must be > 0 when fragCount > 1
						fragMsgs := fragUDPMessage(msg, int(errSize))
						for _, fragMsg := range fragMsgs {
							msgBuf.Reset()
							_ = struc.Pack(&msgBuf, &fragMsg)
							_ = c.CC.SendMessage(msgBuf.Bytes())
						}
					}
				}
				if c.DownCounter != nil {
					c.DownCounter.Add(float64(n))
				}
			}
			if err != nil {
				break
			}
		}
		_ = stream.Close()
	}()

	// Hold the stream until it's closed by the client
	buf := make([]byte, 1024)
	for {
		_, err = stream.Read(buf)
		if err != nil {
			break
		}
	}
	c.CUDPErrorFunc(c.ClientAddr(), c.Auth, id, err)

	// Remove the session
	c.udpSessionMutex.Lock()
	delete(c.udpSessionMap, id)
	c.udpSessionMutex.Unlock()
}
