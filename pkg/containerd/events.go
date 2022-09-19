/*
Copyright © 2022 SUSE LLC
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

// Package containerd handles port binding events from containerd API
package containderd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/Masterminds/log-go"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/namespaces"
	"github.com/docker/go-connections/nat"
	"github.com/gogo/protobuf/proto"
	"github.com/rancher-sandbox/rancher-desktop-agent/pkg/tracker"
)

const portsKey = "nerdctl/ports"

// EventMonitor monitors the Containerd API
// for container events.
type EventMonitor struct {
	containerdClient *containerd.Client
	portTracker      *tracker.PortTracker
}

// NewEventMonitor creates and returns a new Event Monitor for
// Containerd API. Caller is responsible to make sure that
// Docker engine is up and running.
func NewEventMonitor(containerdSock string, portTracker *tracker.PortTracker) (*EventMonitor, error) {
	client, err := containerd.New(containerdSock, containerd.WithDefaultNamespace(namespaces.Default))
	if err != nil {
		return nil, err
	}

	return &EventMonitor{
		containerdClient: client,
		portTracker:      portTracker,
	}, nil
}

// MonitorPorts subscribes to event API
// for container Create/Update/Delete events.
func (e *EventMonitor) MonitorPorts(ctx context.Context) {
	msgCh, errCh := e.containerdClient.Subscribe(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Errorf("context cancellation: %v", ctx.Err())

			return
		case envelope := <-msgCh:
			log.Debugf("received an event: %+v", envelope.Topic)

			switch envelope.Topic {
			case "/containers/create":
				ccEvent := &events.ContainerCreate{}
				err := proto.Unmarshal(envelope.Event.Value, ccEvent)
				if err != nil {
					log.Errorf("failed unmarshaling container create event: %w", err)
				}

				ports, err := e.createPortMapping(ctx, ccEvent.ID)
				if err != nil {
					log.Errorf("failed to create port mapping from container create event: %w", err)
				}

				if err = e.portTracker.Add(ccEvent.ID, ports); err != nil {
					log.Errorf("adding port mapping to tracker failed: %w", err)
				}
			case "/containers/update":
				cuEvent := &events.ContainerUpdate{}
				err := proto.Unmarshal(envelope.Event.Value, cuEvent)
				if err != nil {
					log.Errorf("failed unmarshaling container update event: %w", err)
				}

				ports, err := e.createPortMapping(ctx, cuEvent.ID)
				if err != nil {
					log.Errorf("failed to create port mapping from container update event: %w", err)
				}

				existingPortMap := e.portTracker.Get(cuEvent.ID)
				if existingPortMap != nil {
					if !reflect.DeepEqual(ports, existingPortMap) {
						err := e.portTracker.Remove(cuEvent.ID)
						if err != nil {
							log.Errorf("failed to remove port mapping from container update event: %w", err)
							err := e.portTracker.Add(cuEvent.ID, ports)
							if err != nil {
								log.Errorf("failed to add port mapping from container update event: %w", err)
							}

							continue
						}
					}
				}
				// Not 100% sure if we ever get here...
				if err = e.portTracker.Add(cuEvent.ID, ports); err != nil {
					log.Errorf("failed to add port mapping from container update event: %w", err)
				}

			case "/containers/delete":
				cdEvent := &events.ContainerDelete{}
				err := proto.Unmarshal(envelope.Event.Value, cdEvent)
				if err != nil {
					log.Errorf("failed unmarshaling container delete event: %w", err)
				}

				err = e.portTracker.Remove(cdEvent.ID)
				if err != nil {
					log.Errorf("removing port mapping from tracker failed: %w", err)
				}
			}

		case err := <-errCh:
			log.Errorf("receiving container event failed: %v", err)

			return
		}
	}
}

// IsServing returns true if the client can successfully connect to the
// containerd daemon and the healthcheck service returns the SERVING
// response.
// This call will block if a transient error is encountered during
// connection. A timeout can be set in the context to ensure it returns
// early.
func (e *EventMonitor) IsServing(ctx context.Context) error {
	serving, err := e.containerdClient.IsServing(ctx)
	if serving {
		return nil
	}

	return fmt.Errorf("containerd API is not serving: %w", err)
}

// Close closes the client connection to the API server.
func (e *EventMonitor) Close() error {
	return e.containerdClient.Close()
}

func (e *EventMonitor) createPortMapping(ctx context.Context, containerID string) (nat.PortMap, error) {
	container, err := e.containerdClient.ContainerService().Get(ctx, containerID)
	if err != nil {
		return nil, err
	}

	var ports []Port
	err = json.Unmarshal([]byte(container.Labels[portsKey]), &ports)
	if err != nil {
		return nil, err
	}

	portMap := make(nat.PortMap)

	for _, port := range ports {
		portMapKey, err := nat.NewPort(port.Protocol, strconv.Itoa(port.ContainerPort))
		if err != nil {
			return nil, err
		}

		portBinding := nat.PortBinding{
			HostIP:   port.HostIP,
			HostPort: strconv.Itoa(port.HostPort),
		}
		if pb, ok := portMap[portMapKey]; ok {
			portMap[portMapKey] = append(pb, portBinding)
		} else {
			portMap[portMapKey] = []nat.PortBinding{portBinding}
		}
	}

	return portMap, nil
}

// Port is representing nerdctl/ports entry in the
// evnet envelope's labels.
type Port struct {
	HostPort      int
	ContainerPort int
	Protocol      string
	HostIP        string
}
