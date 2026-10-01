package stack

import (
	"fmt"
	"net"
)

// freePorts asks the kernel for n distinct free ports.
//
// Every listener stays open until all n are taken, so the kernel cannot hand
// the same port out twice. They are closed before returning, which leaves a
// window in which another process could take one; the app's health check is
// what catches that, by failing to come up.
func freePorts(n int) ([]int, error) {
	listeners := make([]net.Listener, 0, n)

	// A listener that fails to close still frees its port when rig exits,
	// and the port numbers are what this returns.
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	ports := make([]int, 0, n)

	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("finding a free port: %w", err)
		}

		listeners = append(listeners, l)

		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			return nil, fmt.Errorf("finding a free port: unexpected address %s", l.Addr())
		}

		ports = append(ports, addr.Port)
	}

	return ports, nil
}
