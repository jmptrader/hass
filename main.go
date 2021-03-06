package main

import (
	"flag"
	"net"
	"sync"
	"time"
)

type ConnTrack struct {
	LocalLocalAddr   string
	LocalRemoteAddr  string
	RemoteLocalAddr  string
	RemoteRemoteAddr string
	Target           string
	Latency          int64
}

type Proxyer struct {
	cfg        *Config
	lock       *sync.Mutex
	ConnTracks map[int]*ConnTrack
	ConnTotal  int
}

func NewProxyer(config *Config) *Proxyer {
	return &Proxyer{
		cfg:        config,
		lock:       new(sync.Mutex),
		ConnTracks: make(map[int]*ConnTrack, 1000),
		ConnTotal:  0,
	}
}

func (p *Proxyer) ConnCount() int {
	return len(p.ConnTracks)
}

func (p *Proxyer) pushConnPair(conntrack *ConnTrack) int {
	p.lock.Lock()
	defer p.lock.Unlock()

	connId := p.ConnTotal
	p.ConnTotal++
	p.ConnTracks[connId] = conntrack
	return connId
}

func (p *Proxyer) popConnPair(connId int) {
	p.lock.Lock()
	defer p.lock.Unlock()
	delete(p.ConnTracks, connId)
}

// Hass's version of socks5 server:
// pick up a backend shadowsocks server then pipe source and server.
func (p *Proxyer) DoProxy(tgt *Target, conn net.Conn) error {

	startAt := time.Now()
	targetAddr := tgt.Addr()

	ssConn, server, err := ConnBackend(p.cfg, tgt)
	if err != nil {
		return err
	}
	defer ssConn.Close()

	latency := int64(time.Since(startAt) / time.Millisecond)
	Debugf("Proxy %v => %v (%vms)", server, targetAddr, latency)

	connTrack := &ConnTrack{
		LocalLocalAddr:   conn.(*net.TCPConn).LocalAddr().String(),
		LocalRemoteAddr:  conn.(*net.TCPConn).RemoteAddr().String(),
		RemoteLocalAddr:  ssConn.LocalAddr().String(),
		RemoteRemoteAddr: ssConn.RemoteAddr().String(),
		Target:           targetAddr,
		Latency:          latency,
	}
	connId := p.pushConnPair(connTrack)
	defer p.popConnPair(connId)

	server.IncreseConnCount()
	defer server.DecreseConnCount()

	timeout := p.cfg.Backend.Timeout

	inChan := make(chan int64, 1)
	outChan := make(chan int64, 1)

	if tgt.request != nil {
		tgt.request.Write(ssConn)
	}

	go CopyNetIO(ssConn, conn, inChan, "client => shawdowsocks", timeout)
	go CopyNetIO(conn, ssConn, outChan, "shawdowsocks => client", timeout)

	for i := 0; i < 2; i++ {
		select {
		case inBytes := <-inChan:
			server.AddInBytes(inBytes)
		case outBytes := <-outChan:
			server.AddOutBytes(outBytes)
		}
	}

	return nil
}

func main() {
	configFile := flag.String("config", "hass.yaml", "Hass default config file (yaml)")
	verbose := flag.Bool("verbose", false, "Default logging level is ERROR, change to DEBUG.")
	flag.Parse()

	if *verbose {
		SetLogLevel(DEBUG)
	}

	config, err := ParseConfigFile(*configFile)
	if err != nil {
		Fatalln("Parse config file failed: ", err)
	}
	config.Report()

	Setrlimit()
	ConfigBackend(config)

	proxy := NewProxyer(config)
	admin := &ProxyAdmin{
		cfg:   config,
		Proxy: proxy,
	}
	go admin.StartSampling()
	go admin.ServeHTTP()

	httpp := &HTTPProxy{
		IPAddr:  config.Local.Host,
		Port:    config.Local.HttpPort,
		Handler: proxy.DoProxy,
	}

	go httpp.Serve()

	socks := &Socks5{
		Ipaddr:  config.Local.Host,
		Port:    config.Local.SocksPort,
		Handler: proxy.DoProxy,
	}

	socks.Serve()
}
