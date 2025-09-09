package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/tinyrange/wireguard"
)

type NetHandler interface {
	wireguard.NetHandler
	Hostname() string
	String() string
	IpAddress() net.IP
}

type ReplaceFunc func(string) (string, error)

type FlowListener interface {
	AcceptConn(source FlowInstance, target FlowInstance, service FlowService, conn net.Conn)
	DialContext(ctx context.Context, target FlowInstance) (net.Conn, error)
}

type FuncFlowListener struct {
	accept func(FlowInstance, FlowInstance, FlowService, net.Conn)
	dial   func(context.Context) (net.Conn, error)
}

// DialContext implements FlowListener.
func (f FuncFlowListener) DialContext(ctx context.Context, target FlowInstance) (net.Conn, error) {
	return f.dial(ctx)
}

// AcceptConn implements FlowListener.
func (f FuncFlowListener) AcceptConn(source FlowInstance, target FlowInstance, service FlowService, conn net.Conn) {
	f.accept(source, target, service, conn)
}

var (
	_ FlowListener = &FuncFlowListener{}
)

// Flows are formatted as "tag/instance:service".
type ParsedFlow struct {
	Tag      string
	Instance string
	Service  string
}

func ParseFlow(flow string, replace ReplaceFunc) (ParsedFlow, error) {
	parts := strings.Split(flow, ":")
	if len(parts) != 2 {
		return ParsedFlow{}, fmt.Errorf("invalid flow: %s", flow)
	}

	tagParts := strings.Split(parts[0], "/")
	if len(tagParts) == 2 {
		if strings.HasPrefix(tagParts[1], "{") {
			if !strings.HasSuffix(tagParts[1], "}") {
				return ParsedFlow{}, fmt.Errorf("invalid tag: %s", tagParts[1])
			}
			if replace == nil {
				return ParsedFlow{}, fmt.Errorf("replace function required for dynamic instance")
			}
			tag := tagParts[1][1 : len(tagParts[1])-1]
			replaceStr, err := replace(tag)
			if err != nil {
				return ParsedFlow{}, err
			}
			tagParts[1] = replaceStr
		}

		return ParsedFlow{
			Tag:      tagParts[0],
			Instance: tagParts[1],
			Service:  parts[1],
		}, nil
	} else {
		return ParsedFlow{}, fmt.Errorf("invalid flow: %s", flow)
	}
}

func ParseTag(tag string, replace ReplaceFunc) (string, error) {
	tagParts := strings.Split(tag, "/")
	if len(tagParts) == 2 {
		if strings.HasPrefix(tagParts[1], "{") {
			if !strings.HasSuffix(tagParts[1], "}") {
				return "", fmt.Errorf("invalid tag: %s", tagParts[1])
			}
			if replace == nil {
				return "", fmt.Errorf("replace function required for dynamic instance")
			}
			tag := tagParts[1][1 : len(tagParts[1])-1]
			replaceStr, err := replace(tag)
			if err != nil {
				return "", err
			}
			tagParts[1] = replaceStr
		}

		return fmt.Sprintf("%s/%s", tagParts[0], tagParts[1]), nil
	} else {
		return "", fmt.Errorf("invalid tag: %s", tag)
	}
}

func (flow ParsedFlow) String() string {
	if flow.Instance == "" {
		return fmt.Sprintf("%s:%s", flow.Tag, flow.Service)
	}
	return fmt.Sprintf("%s/%s:%s", flow.Tag, flow.Instance, flow.Service)
}

type FlowList []string

func (flows FlowList) Parse(replace ReplaceFunc) ([]ParsedFlow, error) {
	var parsedFlows []ParsedFlow

	for _, flow := range flows {
		parsedFlow, err := ParseFlow(flow, replace)
		if err != nil {
			return nil, err
		}
		parsedFlows = append(parsedFlows, parsedFlow)
	}

	return parsedFlows, nil
}

type FlowService interface {
	FlowListener
	Name() string
	Port() int
	Tags() TagList
}

type FlowInstance interface {
	Hostname() string
	Tags() TagList
	InstanceAddress() net.IP
	Services() []FlowService
	Flows() []ParsedFlow
}

type flowRouterHandler struct {
	router   *FlowRouter
	instance FlowInstance
}

// String implements NetHandler.
func (h *flowRouterHandler) String() string {
	return h.Hostname()
}

// IpAddress implements NetHandler.
func (h *flowRouterHandler) IpAddress() net.IP {
	return h.instance.InstanceAddress()
}

// String implements NetHandler.
func (h *flowRouterHandler) Hostname() string {
	return h.instance.Hostname()
}

// HandleConn implements NetHandler.
func (f *flowRouterHandler) HandleConn(network string, ip net.IP, port uint16, conn net.Conn) {
	slog.Debug("handling connection", "source", f.instance.Hostname(), "ip", ip, "port", port)
	slog.Info("handling connection", "source", conn.RemoteAddr(), "dest", conn.LocalAddr())

	// find the target by IP
	f.router.mtx.RLock()
	for _, inst := range f.router.instances {
		target := inst.instance

		if target.InstanceAddress().Equal(ip) {
			// We found the target, now find the service.
			for _, service := range target.Services() {
				slog.Debug("checking service", "port", service.Port())
				if service.Port() == int(port) {
					f.router.mtx.RUnlock()
					// We now know instance is trying to connect to target:service.
					slog.Debug("found target", "source", f.instance.Hostname(), "target", target.Hostname(), "service", service.Name())
					err := f.router.handleConnection(f.instance, target, service, conn)
					if err != nil {
						slog.Warn("failed to handle connection (found service)", "err", err)
						conn.Close()
					}
					return
				}
			}

			slog.Warn(fmt.Sprintf("no service matches port %d on instance %s", port, target.Hostname()))
		}
	}

	f.router.mtx.RUnlock()

	// If we reach here, we couldn't find the target.
	// TODO(joshua): Handle a default route.
	slog.Warn("no matching target", "source", f.instance.Hostname(), "ip", ip, "port", port)
	conn.Close()
}

var (
	_ NetHandler = &flowRouterHandler{}
)

type FlowRouter struct {
	mtx       sync.RWMutex
	instances map[string]*flowRouterHandler
}

func isRequestAllowed(sourceFlows []ParsedFlow, targetTags TagList, serviceTags TagList) bool {
	// Iterate though each flow.
	for _, flow := range sourceFlows {
		// Check if the target tags match.
		if flow.Instance == "*" {
			if flow.Tag != "*" && !targetTags.ContainsMatchingPrefix(fmt.Sprintf("%s/", flow.Tag)) {
				slog.Debug("rejected target partial", "flow", flow, "targetTags", targetTags)
				continue
			}
		} else if !targetTags.Contains(fmt.Sprintf("%s/%s", flow.Tag, flow.Instance)) {
			slog.Debug("rejected target", "flow", flow, "targetTags", targetTags)
			continue
		}

		// Check if the service tags match.
		if flow.Service != "*" && !serviceTags.Contains(flow.Service) {
			slog.Debug("rejected service", "flow", flow, "serviceTags", serviceTags)
			continue
		}

		return true
	}

	return false
}

func (r *FlowRouter) handleConnection(source FlowInstance, target FlowInstance, service FlowService, conn net.Conn) error {
	if !isRequestAllowed(source.Flows(), target.Tags(), service.Tags()) {
		return fmt.Errorf("request blocked (no matching flow), source=%s, target=%s, service=%s", source.Hostname(), target.Hostname(), service.Name())
	}

	service.AcceptConn(source, target, service, conn)
	return nil
}

func (r *FlowRouter) dialContext(ctx context.Context, source FlowInstance, target FlowInstance, service FlowService, network, address string) (net.Conn, error) {
	// If source is nil, we are dialing from the router itself.
	if source != nil && !isRequestAllowed(source.Flows(), target.Tags(), service.Tags()) {
		return nil, fmt.Errorf("no matching flow: %s", address)
	}

	return service.DialContext(ctx, target)
}

func (r *FlowRouter) DialContext(ctx context.Context, source FlowInstance, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("unsupported network: %s", network)
	}

	slog.Debug("dialing", "source", source, "network", network, "address", address)

	// Parse the address.
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("failed to parse address: %w", err)
	}

	// We manually unlock on each exit path so that we can unlock before actually
	// doing the dialing (so that we don't hold the lock for longer than necessary).
	r.mtx.RLock()

	// Find the target instance.
	for _, inst := range r.instances {
		target := inst.instance

		slog.Debug("checking target", "target", target.Hostname(), "instanceAddress", target.InstanceAddress().String(), "host", host)

		if target.Hostname() == host {
			// We found the target, now find the service.
			for _, service := range target.Services() {
				if strconv.Itoa(service.Port()) == port || service.Name() == port {
					// We now know instance is trying to connect to target:service.
					r.mtx.RUnlock()
					return r.dialContext(ctx, source, target, service, network, address)
				}
			}
		}

		if target.InstanceAddress().String() == host {
			// We found the target, now find the service.
			for _, service := range target.Services() {
				if strconv.Itoa(service.Port()) == port || service.Name() == port {
					// We now know instance is trying to connect to target:service.
					r.mtx.RUnlock()
					return r.dialContext(ctx, source, target, service, network, address)
				}
			}
		}
	}

	r.mtx.RUnlock()
	return nil, fmt.Errorf("no matching target: %s -> %s", source, address)
}

func (r *FlowRouter) AddInstance(instance FlowInstance) (NetHandler, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	if _, ok := r.instances[instance.Hostname()]; ok {
		return nil, fmt.Errorf("instance already exists: %s", instance.Hostname())
	}

	handler := &flowRouterHandler{
		router:   r,
		instance: instance,
	}

	r.instances[instance.Hostname()] = handler

	return handler, nil
}

func NewFlowRouter() *FlowRouter {
	return &FlowRouter{
		instances: make(map[string]*flowRouterHandler),
	}
}
