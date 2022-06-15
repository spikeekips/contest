package contest

import (
	"sync"

	"github.com/pkg/errors"
)

type Hosts struct {
	sync.Mutex
	hostids          []string
	hosts            map[string]Host
	hostsbycontainer map[string]Host
	containersbyhost map[string][]string
	lastused         int
}

func NewHosts() *Hosts {
	return &Hosts{
		hosts:            map[string]Host{},
		hostsbycontainer: map[string]Host{},
		containersbyhost: map[string][]string{},
		lastused:         -1,
	}
}

func (h *Hosts) Close() error {
	for i := range h.hosts {
		if err := h.hosts[i].Close(); err != nil {
			return err
		}
	}

	return nil
}

func (h *Hosts) Len() int {
	return len(h.hosts)
}

func (h *Hosts) New(ho Host) error {
	if _, found := h.hosts[ho.Address()]; found {
		return errors.Errorf("already added")
	}

	h.hostids = append(h.hostids, ho.Address())
	h.hosts[ho.Address()] = ho

	return nil
}

func (h *Hosts) NewContainer(cid string) (Host, error) {
	ho := h.findHost()
	if ho == nil {
		return nil, errors.Errorf("failed to find host")
	}

	h.hostsbycontainer[cid] = ho
	h.containersbyhost[ho.Address()] = append(h.containersbyhost[ho.Address()], cid)

	return ho, nil
}

func (h *Hosts) Host(hostaddress string) Host {
	return h.hosts[hostaddress]
}

func (h *Hosts) HostByContainer(cid string) Host {
	return h.hostsbycontainer[cid]
}

func (h *Hosts) Traverse(f func(Host) (bool, error)) error {
	for addr := range h.hosts {
		host := h.hosts[addr]

		switch keep, err := f(host); {
		case err != nil:
			return err
		case !keep:
			return nil
		}
	}

	return nil
}

func (h *Hosts) TraverseByHost(f func(_ Host, cids []string) (bool, error)) error {
	for addr := range h.containersbyhost {
		ho := h.Host(addr)

		switch keep, err := f(ho, h.containersbyhost[addr]); {
		case err != nil:
			return err
		case !keep:
			return nil
		}
	}

	return nil
}

func (h *Hosts) findHost() Host {
	h.Lock()
	defer h.Unlock()

	if len(h.hostids) < 1 {
		return nil
	}

	index := h.lastused + 1
	if index == len(h.hostids) {
		index = 0
	}

	h.lastused = index

	return h.hosts[h.hostids[index]]
}
