package contest

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/docker/docker/api/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

var DefaultHostBase = "/tmp/contest"

var supportedArchs = map[string]elf.Machine{
	"Linux x86_64":  elf.EM_X86_64,
	"Linux aarch64": elf.EM_AARCH64,
}

var supportedArchsStrings = map[elf.Machine]string{
	elf.EM_386:     "linux/386",
	elf.EM_X86_64:  "linux/amd64",
	elf.EM_ARM:     "linux/arm",
	elf.EM_AARCH64: "linux/arm64",
	elf.EM_MIPS:    "linux/mips",
	elf.EM_PPC64:   "linux/ppc64",
	elf.EM_RISCV:   "linux/riscv64",
	elf.EM_S390:    "linux/s390x",
}

type Host interface {
	Arch() elf.Machine
	User() string
	Address() string
	Hostname() string
	PublishHost() string
	SetPublishHost(string)
	Base() string
	Close() error
	Client() *dockerClient.Client
	Mkdir(string, os.FileMode) error
	Upload(io.Reader, string, os.FileMode) error
	ContainerFreePort(string, string, string) (string, error)
	CreateContainer(
		_ context.Context,
		_ *container.Config,
		_ *container.HostConfig,
		_ *network.NetworkingConfig,
		containerName string,
	) error
	StartContainer(
		_ context.Context,
		_ *container.Config,
		_ *container.HostConfig,
		_ *network.NetworkingConfig,
		containerName string,
		whenExit func(container.ContainerWaitOKBody, error),
	) error
	StopContainer(_ context.Context, containerName string, _ *time.Duration) error
	RemoveContainer(_ context.Context, containerName string, _ dockerTypes.ContainerRemoveOptions) error
	ContainerLogs(_ context.Context, containerName string, _ types.ContainerLogsOptions) (io.ReadCloser, error)
	PortMap(string) nat.PortMap
	FreePort(string) (string, error)
}

func MachineToString(m elf.Machine) string {
	s, found := supportedArchsStrings[m]
	if !found {
		return fmt.Sprintf("unknown, %q", m)
	}

	return s
}

type Artifact struct {
	Source   []byte
	Target   string
	FileMode os.FileMode
}
