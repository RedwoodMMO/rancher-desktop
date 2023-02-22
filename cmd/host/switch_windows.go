/*
Copyright © 2023 SUSE LLC
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/dustin/go-humanize"
	"github.com/rancher-sandbox/rancher-desktop-host-resolver/pkg/vmsock"
	"github.com/rancher-sandbox/rancher-desktop-networking/pkg/vsock"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

var (
	debug             bool
	virtualSubnet     string
	staticPortForward arrayFlags
)

const (
	vsockListenPort    = 6655
	vsockHandshakePort = 6669
	seedPhrase         = "github.com/rancher-sandbox/rancher-desktop-networking"
	timeoutSeconds     = 5 * 60
)

func main() {
	flag.BoolVar(&debug, "debug", false, "enable additional debugging")
	flag.StringVar(&virtualSubnet, "subnet", defaultSubnet,
		fmt.Sprintf("Subnet range with CIDR suffix for virtual network, e,g: %s", defaultSubnet))
	flag.Var(&staticPortForward, "port-forward",
		"List of ports that needs to be pre forwarded to the WSL VM in Host:Port=Guest:Port format e.g: 127.0.0.1:2222=192.168.127.2:22")
	flag.Parse()

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// config flags
	ctx, cancel := context.WithCancel(context.Background())
	groupErrs, ctx := errgroup.WithContext(ctx)

	// catch user issued signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	subnet, err := validateSubnet(virtualSubnet)
	if err != nil {
		logrus.Fatal(err)
	}

	portForwarding, err := parsePortForwarding(staticPortForward)
	if err != nil {
		logrus.Fatal(err)
	}

	config := newConfig(*subnet, portForwarding, debug)

	ln, err := vsockListener(vsockHandshakePort, vsockListenPort, seedPhrase)
	if err != nil {
		logrus.Fatalf("creating vsock listener for hostSwitch failed: %v", err)
	}
	logrus.Debugf("attempting to start a virtual network with the following config: %+v", config)
	groupErrs.Go(func() error {
		return run(ctx, groupErrs, &config, ln)
	})

	// Wait for something to happen
	groupErrs.Go(func() error {
		select {
		// Catch signals so exits are graceful and defers can run
		case s := <-sigChan:
			cancel()
			return fmt.Errorf("signal caught: %v", s)
		case <-ctx.Done():
			return nil
		}
	})
	// Wait for all of the go funcs to finish up
	if err := groupErrs.Wait(); err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, g *errgroup.Group, config *types.Configuration, ln net.Listener) error {
	vn, err := virtualnetwork.New(config)
	if err != nil {
		return err
	}
	logrus.Info("waiting for clients...")
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				logrus.Errorf("failed to accept: %v", err)
			}
			// AcceptQemu calls the underlying virtual network switch Accept function
			err = vn.AcceptQemu(ctx, conn)
			if err != nil {
				logrus.Errorf("failed to accept connection: %v", err)
			} else {
				logrus.Infof("accepted connection: ctx=%+v conn=%+v", ctx, conn)
			}
		}
	}()

	apiServer := fmt.Sprintf("%s:80", config.GatewayIP)
	vnLn, err := vn.Listen("tcp", apiServer)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/services/forwarder/all", vn.Mux())
	mux.Handle("/services/forwarder/expose", vn.Mux())
	mux.Handle("/services/forwarder/unexpose", vn.Mux())
	httpServe(ctx, g, vnLn, mux)
	logrus.Infof("port forwarding API server is running on: %s", apiServer)

	logInterval := time.Second * 5
	if debug {
		g.Go(func() error {
		debugLog:
			for {
				select {
				case <-time.After(logInterval):
					logrus.Debugf("%v sent to the VM, %v received from the VM", humanize.Bytes(vn.BytesSent()), humanize.Bytes(vn.BytesReceived()))
				case <-ctx.Done():
					break debugLog
				}
			}
			return nil
		})
	}
	return nil
}

func httpServe(ctx context.Context, g *errgroup.Group, ln net.Listener, mux http.Handler) {
	g.Go(func() error {
		<-ctx.Done()
		return ln.Close()
	})
	g.Go(func() error {
		s := &http.Server{
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		err := s.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
}

func vsockListener(handshakePort, vsockListenPort uint32, seed string) (net.Listener, error) {
	bailout := time.After(time.Second * timeoutSeconds)
	vmGUID, err := vsock.GetVMGUID(seed, handshakePort, bailout)
	if err != nil {
		return nil, fmt.Errorf("trying to find WSL GUID faild: %w", err)
	}
	logrus.Infof("successful handshake, listening for incoming connection from: %v", vmGUID.String())

	return vmsock.Listen(vmGUID, vsockListenPort)
}
