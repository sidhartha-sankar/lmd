package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Listener is the object which handles incoming connections
type Listener struct {
	noCopy           noCopy
	ConnectionString string
	shutdownChannel  chan bool
	connection       net.Listener
	LocalConfig      *Config
	waitGroupDone    *sync.WaitGroup
	waitGroupInit    *sync.WaitGroup
}

// NewListener creates a new Listener object
func NewListener(LocalConfig *Config, listen string, waitGroupInit *sync.WaitGroup, waitGroupDone *sync.WaitGroup, shutdownChannel chan bool) *Listener {
	l := Listener{
		ConnectionString: listen,
		shutdownChannel:  shutdownChannel,
		LocalConfig:      LocalConfig,
		waitGroupDone:    waitGroupDone,
		waitGroupInit:    waitGroupInit,
	}
	go func() {
		defer logPanicExit()
		l.Listen()
	}()
	return &l
}

// QueryServer handles a single client connection.
// It returns any error encountered.
func QueryServer(c net.Conn) error {
	localAddr := c.LocalAddr().String()
	keepAlive := false
	remote := c.RemoteAddr().String()
	defer c.Close()
	if remote == "" {
		remote = "unknown"
	}

	for {
		if !keepAlive {
			promFrontendConnections.WithLabelValues(localAddr).Inc()
			log.Debugf("incoming request from: %s to %s", remote, localAddr)
			c.SetDeadline(time.Now().Add(time.Duration(10) * time.Second))
		}

		reqs, err := ParseRequests(c)
		if err != nil {
			if err, ok := err.(net.Error); ok {
				if keepAlive {
					log.Debugf("closing keepalive connection from %s", remote)
				} else {
					log.Debugf("network error from %s: %s", remote, err.Error())
				}
				return err
			}
			(&Response{Code: 400, Request: &Request{}, Error: err}).Send(c)
			return err
		}
		if len(reqs) > 0 {
			keepAlive, err = ProcessRequests(reqs, c, remote)

			// keep open keepalive request until either the client closes the connection or the deadline timeout is hit
			if keepAlive {
				log.Debugf("keepalive connection from %s, waiting for more requests", remote)
				c.SetDeadline(time.Now().Add(time.Duration(10) * time.Second))
				continue
			}
		} else if keepAlive {
			// wait up to deadline after the last keep alive request
			time.Sleep(100 * time.Millisecond)
			continue
		} else {
			err = errors.New("bad request: empty request")
			(&Response{Code: 400, Request: &Request{}, Error: err}).Send(c)
			return err
		}

		return err
	}
}

// ProcessRequests creates response for all given requests
func ProcessRequests(reqs []*Request, c net.Conn, remote string) (bool, error) {
	if len(reqs) == 0 {
		return false, nil
	}
	commandsByPeer := make(map[string][]string)
	for _, req := range reqs {
		t1 := time.Now()
		if req.Command != "" {
			for _, pID := range req.BackendsMap {
				commandsByPeer[pID] = append(commandsByPeer[pID], strings.TrimSpace(req.Command))
			}
		} else {
			// send all pending commands so far
			if len(commandsByPeer) > 0 {
				SendCommands(&commandsByPeer)
				commandsByPeer = make(map[string][]string)
				log.Infof("incoming command request from %s to %s finished in %s", remote, c.LocalAddr().String(), time.Since(t1))
			}
			if req.WaitTrigger != "" {
				c.SetDeadline(time.Now().Add(time.Duration(req.WaitTimeout+1000) * time.Millisecond))
			}
			if req.Table == "log" {
				c.SetDeadline(time.Now().Add(time.Duration(60) * time.Second))
			}
			response, rErr := req.GetResponse()
			if rErr != nil {
				if netErr, ok := rErr.(net.Error); ok {
					(&Response{Code: 500, Request: req, Error: netErr}).Send(c)
					return false, netErr
				}
				(&Response{Code: 400, Request: req, Error: rErr}).Send(c)
				return false, rErr
			}

			size, sErr := response.Send(c)
			duration := time.Since(t1)
			log.Infof("incoming %s request from %s to %s finished in %s, size: %.3f kB", req.Table, remote, c.LocalAddr().String(), duration.String(), float64(size)/1024)
			if sErr != nil {
				return false, sErr
			}
			if !req.KeepAlive {
				return false, nil
			}
		}
	}

	// send all remaining commands
	if len(commandsByPeer) > 0 {
		t1 := time.Now()
		SendCommands(&commandsByPeer)
		log.Infof("incoming command request from %s to %s finished in %s", remote, c.LocalAddr().String(), time.Since(t1))
	}

	return reqs[len(reqs)-1].KeepAlive, nil
}

// SendCommands sends commands for this request to all selected remote sites.
// It returns any error encountered.
func SendCommands(commandsByPeer *map[string][]string) {
	wg := &sync.WaitGroup{}
	for pID := range *commandsByPeer {
		PeerMapLock.RLock()
		p := PeerMap[pID]
		PeerMapLock.RUnlock()
		wg.Add(1)
		go func(peer *Peer) {
			// make sure we log panics properly
			defer wg.Done()
			defer logPanicExitPeer(peer)
			commandRequest := &Request{
				Command: strings.Join((*commandsByPeer)[peer.ID], "\n\n"),
			}
			peer.PeerLock.Lock()
			peer.Status["LastQuery"] = time.Now().Unix()
			if peer.Status["Idling"].(bool) {
				peer.Status["Idling"] = false
				log.Infof("[%s] switched back to normal update interval", peer.Name)
			}
			peer.PeerLock.Unlock()
			_, err := peer.Query(commandRequest)
			if err != nil {
				log.Warnf("[%s] sending command failed: %s", peer.Name, err.Error())
				return
			}
			log.Infof("[%s] send %d commands successfully.", peer.Name, len((*commandsByPeer)[peer.ID]))

			// schedule immediate update
			peer.ScheduleImmediateUpdate()
		}(p)
	}
	// Wait up to 10 seconds for all commands being sent
	waitTimeout(wg, 10)
}

// Listen start listening the actual connection
func (l *Listener) Listen() {
	defer func() {
		ListenersLock.Lock()
		delete(Listeners, l.ConnectionString)
		ListenersLock.Unlock()
		l.waitGroupDone.Done()
	}()
	l.waitGroupDone.Add(1)
	listen := l.ConnectionString
	if strings.HasPrefix(listen, "https://") {
		listen = strings.TrimPrefix(listen, "https://")
		l.LocalListenerHTTP("https", listen)
	} else if strings.HasPrefix(listen, "http://") {
		listen = strings.TrimPrefix(listen, "http://")
		l.LocalListenerHTTP("http", listen)
	} else if strings.HasPrefix(listen, "tls://") {
		listen = strings.TrimPrefix(listen, "tls://")
		listen = strings.TrimPrefix(listen, "*") // * means all interfaces
		l.LocalListenerLivestatus("tls", listen)
	} else if strings.Contains(listen, ":") {
		listen = strings.TrimPrefix(listen, "*") // * means all interfaces
		l.LocalListenerLivestatus("tcp", listen)
	} else {
		// remove stale sockets on start
		if _, err := os.Stat(listen); err == nil {
			log.Warnf("removing stale socket: %s", listen)
			os.Remove(listen)
		}
		l.LocalListenerLivestatus("unix", listen)
	}
}

// LocalListenerLivestatus starts a listening socket with livestatus protocol.
func (l *Listener) LocalListenerLivestatus(connType string, listen string) {
	var err error
	var c net.Listener
	if connType == "tls" {
		tlsConfig, tErr := getTLSListenerConfig(l.LocalConfig)
		if tErr != nil {
			log.Fatalf("failed to initialize tls %s", tErr.Error())
		}
		c, err = tls.Listen("tcp", listen, tlsConfig)
	} else {
		c, err = net.Listen(connType, listen)
	}
	l.connection = c
	if err != nil {
		log.Fatalf("listen error: %s", err.Error())
		return
	}
	defer c.Close()
	if connType == "unix" {
		defer os.Remove(listen)
	}
	defer log.Infof("%s listener %s shutdown complete", connType, listen)
	log.Infof("listening for incoming queries on %s %s", connType, listen)

	// Close connection and log shutdown
	go func() {
		<-l.shutdownChannel
		log.Infof("stopping %s listener on %s", connType, listen)
		c.Close()
	}()

	l.waitGroupInit.Done()

	for {
		fd, err := c.Accept()
		if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
			continue
		}
		if err != nil {
			// connection closed by shutdown channel
			return
		}

		// background waiting for query to finish/timeout
		go func() {
			// process client request with a timeout

			// make sure we log panics properly
			defer logPanicExit()

			ch := make(chan error, 1)
			go func() {
				// make sure we log panics properly
				defer logPanicExit()

				ch <- QueryServer(fd)
			}()
			select {
			case <-ch:
			// request finishes normally
			case <-time.After(time.Duration(l.LocalConfig.ListenTimeout) * time.Second):
				localAddr := fd.LocalAddr().String()
				remote := fd.RemoteAddr().String()
				log.Warnf("client request from %s to %s timed out", remote, localAddr)
			}
			fd.Close()
		}()
	}
}

// LocalListenerHTTP starts a listening socket with http protocol.
func (l *Listener) LocalListenerHTTP(httpType string, listen string) {
	// Parse listener address
	listen = strings.TrimPrefix(listen, "*") // * means all interfaces

	// Listener
	var c net.Listener
	if httpType == "https" {
		tlsConfig, err := getTLSListenerConfig(l.LocalConfig)
		if err != nil {
			log.Fatalf("failed to initialize https %s", err.Error())
		}
		ln, err := tls.Listen("tcp", listen, tlsConfig)
		if err != nil {
			log.Fatalf("listen error: %s", err.Error())
			return
		}
		c = ln
	} else {
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			log.Fatalf("listen error: %s", err.Error())
			return
		}
		c = ln
	}
	l.connection = c

	// Close connection and log shutdown
	go func() {
		<-l.shutdownChannel
		log.Infof("stopping listener on %s", listen)
		c.Close()
	}()

	// Initialize HTTP router
	router, err := initializeHTTPRouter()
	if err != nil {
		log.Fatalf("error initializing http server: %s", err.Error())
		return
	}
	log.Infof("listening for rest queries on %s", listen)
	l.waitGroupInit.Done()

	// Wait for and handle http requests
	server := &http.Server{
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	server.Serve(c)

}

func getTLSListenerConfig(LocalConfig *Config) (config *tls.Config, err error) {
	if LocalConfig.TLSCertificate == "" || LocalConfig.TLSKey == "" {
		log.Fatalf("TLSCertificate and TLSKey configuration items are required for tls connections")
	}
	cer, err := tls.LoadX509KeyPair(LocalConfig.TLSCertificate, LocalConfig.TLSKey)
	if err != nil {
		return nil, err
	}
	config = &tls.Config{Certificates: []tls.Certificate{cer}}
	if len(LocalConfig.TLSClientPems) > 0 {
		caCertPool := x509.NewCertPool()
		for _, file := range LocalConfig.TLSClientPems {
			caCert, err := ioutil.ReadFile(file)
			if err != nil {
				return nil, err
			}
			caCertPool.AppendCertsFromPEM(caCert)
		}

		config.ClientAuth = tls.RequireAndVerifyClientCert
		config.ClientCAs = caCertPool
	}
	return
}
