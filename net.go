package contest

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"sync"

	"github.com/pkg/errors"
)

var (
	bindPortRange = [2]int64{1025, 32767}
	bindPortLock  sync.RWMutex
)

func AvailablePort(network string) (string, error) {
	bindPortLock.Lock()
	defer bindPortLock.Unlock()

	return availablePort(network)
}

func availablePort(network string) (string, error) {
	switch network {
	case "tcp":
		return availableTCPPort()
	case "udp":
		return availableUDPPort()
	default:
		return "", errors.Errorf("unknown network, %q", network)
	}
}

func availableTCPPort() (string, error) {
	switch addr, err := net.ResolveTCPAddr("tcp", "localhost:0"); {
	case err != nil:
		return "", errors.WithStack(err)
	default:
		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			return "", errors.WithStack(err)
		}

		defer func() {
			_ = l.Close()
		}()

		return fmt.Sprintf("%d", l.Addr().(*net.TCPAddr).Port), nil //nolint:forcetypeassert //...
	}
}

func checkAvailableUDPPort(port string) error {
	switch addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%s", port)); {
	case err != nil:
		return errors.WithStack(err)
	default:
		l, err := net.ListenUDP("udp", addr)
		if err != nil {
			return errors.WithStack(err)
		}

		defer func() {
			_ = l.Close()
		}()

		return nil
	}
}

func randPorts() string {
	n, err := rand.Int(rand.Reader, big.NewInt(bindPortRange[1]-bindPortRange[0]))
	if err != nil {
		panic(err)
	}

	i := n.Int64() + bindPortRange[0]

	return fmt.Sprintf("%d", i)
}

func availableUDPPort() (string, error) {
	var port string

	for {
		port = randPorts()

		if err := checkAvailableUDPPort(port); err == nil {
			return port, nil
		}
	}
}
