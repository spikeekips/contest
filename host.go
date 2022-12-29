package contest

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerNetwork "github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
)

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

type Host interface { //nolint:interfacebloat //...
	Arch() elf.Machine
	User() string
	Address() string
	HostID() string
	Hostname() string
	PublishHost() string
	SetPublishHost(string)
	Base() string
	File(name string) (path string, found bool)
	Close() error
	Client() *dockerClient.Client
	Mkdir(string, os.FileMode) error
	Upload(_ io.Reader, name, dest string, _ os.FileMode) error
	CollectResult(outputfile string) error
	ExistsContainer(_ context.Context, containerName string) (string, string, bool, error)
	CreateContainer(
		_ context.Context,
		_ *container.Config,
		_ *container.HostConfig,
		_ *dockerNetwork.NetworkingConfig,
		containerName string,
	) error
	StartContainer(
		_ context.Context,
		_ *container.HostConfig,
		_ *dockerNetwork.NetworkingConfig,
		containerName string,
		whenExit func(container.ContainerWaitOKBody, error),
	) error
	StopContainer(_ context.Context, containerName string, _ *time.Duration) error
	RemoveContainer(_ context.Context, containerName string, _ dockerTypes.ContainerRemoveOptions) error
	ContainerLogs(_ context.Context, containerName string, _ dockerTypes.ContainerLogsOptions) (io.ReadCloser, error)
	FreePort(id, network string) (string, error)
	RunCommand(string) (string, string, bool, error)
}

func MachineToString(m elf.Machine) string {
	s, found := supportedArchsStrings[m]
	if !found {
		return fmt.Sprintf("unknown, %q", m)
	}

	return s
}

type Artifact struct {
	Target   string
	Source   []byte
	FileMode os.FileMode
}

func LoadHostCommandArgs(args []string) (string, error) {
	var cmd string

	for i := range args {
		a := args[i]

		if j := strings.TrimSpace(a); strings.HasPrefix(j, "$ ") {
			cmd = j[2:]

			continue
		}

		if i > 0 {
			cmd += " "
		}

		switch {
		case strings.Contains(" ", a):
			cmd += `'` + strings.Replace(a, "'", "\\'", -1) + `'`
		default:
			cmd += a
		}
	}

	return cmd, nil
}
