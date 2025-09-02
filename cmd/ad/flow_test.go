package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
)

type mockFlow struct {
	tags     TagList
	services []FlowService
	flows    []ParsedFlow
}

func (f mockFlow) Hostname() string {
	return "mock"
}

func (f mockFlow) Tags() TagList {
	return f.tags
}

func (f mockFlow) InstanceAddress() net.IP {
	return net.IP{10, 40, 2, 2}
}

func (f mockFlow) Services() []FlowService {
	return f.services
}

func (f mockFlow) Flows() []ParsedFlow {
	return f.flows
}

type mockService struct {
	name string
	port int
	tags TagList
}

func (s mockService) Name() string {
	return s.name
}
func (s mockService) Port() int {
	return s.port
}
func (s mockService) Tags() TagList {
	return s.tags
}

func (s mockService) AcceptConn(source FlowInstance, target FlowInstance, service FlowService, conn net.Conn) {
}

func (s mockService) DialContext(ctx context.Context, target FlowInstance) (net.Conn, error) {
	return nil, fmt.Errorf("DialContext not implemented on mockService")
}

func mkService(tags TagList) mockService {
	return mockService{
		"service",
		1337,
		tags,
	}
}

const ()

func setup(sourceFlows []ParsedFlow, targetTags TagList, serviceTags TagList) (mockFlow, mockFlow, mockService) {
	source := mockFlow{TagList{}, []FlowService{}, sourceFlows}
	service := mkService(serviceTags)
	target := mockFlow{targetTags, []FlowService{service}, []ParsedFlow{}}
	return source, target, service
}

func isAllowed(sourceFlows []ParsedFlow, targetTags TagList, serviceTags TagList) bool {
	source, target, service := setup(sourceFlows, targetTags, serviceTags)
	return isRequestAllowed(source, target, service)
}

func TestHandleConnection(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	if !isAllowed([]ParsedFlow{{"*", "*", "public"}}, TagList{}, TagList{"public"}) {
		t.Errorf("valid request blocked")
	}

	if isAllowed([]ParsedFlow{{"team", "*", "public"}}, TagList{}, TagList{"public"}) {
		t.Errorf("invalid request allowed")
	}

	if !isAllowed([]ParsedFlow{{"team", "*", "public"}}, TagList{"team/1"}, TagList{"public"}) {
		t.Errorf("valid request blocked")
	}

	if isAllowed([]ParsedFlow{{"team", "*", "private"}}, TagList{"team/1"}, TagList{"public"}) {
		t.Errorf("invalid request allowed")
	}

	if isAllowed([]ParsedFlow{{"team", "1", "public"}}, TagList{"team/2"}, TagList{"public"}) {
		t.Errorf("invalid request allowed")
	}

	if !isAllowed([]ParsedFlow{{"team", "1", "public"}}, TagList{"team/1"}, TagList{"public"}) {
		t.Errorf("valid request blocked")
	}

	if isAllowed([]ParsedFlow{{"team", "1", "public"}}, TagList{"team/1"}, TagList{"private"}) {
		t.Errorf("invalid request allowed")
	}

	if !isAllowed([]ParsedFlow{{"team", "1", "public"}}, TagList{"team/1"}, TagList{"private", "public"}) {
		t.Errorf("valid request blocked")
	}

	// Tag wildcards without instance wildcards are ignored (`*` is treated as a regular tag in this case)
	if isAllowed([]ParsedFlow{{"*", "1", "public"}}, TagList{"team/1"}, TagList{"public"}) {
		t.Errorf("invalid request allowed")
	}
}
