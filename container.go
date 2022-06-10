package contest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	dockerClient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
)

var ContainerLabel = "mitum-contest"

var errContainerLogIgnore = util.NewError("failed to read container logs; ignored")

func TraverseContainers(
	ctx context.Context,
	client *dockerClient.Client,
	callback func(dockerTypes.Container) (bool, error),
) error {
	cs, err := client.ContainerList(
		ctx,
		dockerTypes.ContainerListOptions{
			All: true,
		},
	)
	if err != nil {
		return err
	}

	for i := range cs {
		c := cs[i]

		var found bool
		for k := range c.Labels {
			if strings.HasPrefix(k, ContainerLabel) {
				found = true

				break
			}
		}

		if !found {
			continue
		}

		switch keep, err := callback(c); {
		case err != nil:
			return err
		case !keep:
			return nil
		}
	}

	return nil
}

func ExistsImage(client *dockerClient.Client, image string) (bool, error) {
	i, err := client.ImageList(
		context.Background(),
		dockerTypes.ImageListOptions{
			All: true,
			Filters: filters.NewArgs(filters.KeyValuePair{
				Key:   "reference",
				Value: image,
			}),
		},
	)
	if err != nil {
		return false, err
	}

	return len(i) > 0, nil
}

func PullImage(client *dockerClient.Client, image string) error {
	r, err := client.ImagePull(
		context.Background(),
		image,
		types.ImagePullOptions{},
	)
	if err != nil {
		return errors.Wrap(err, "")
	}

	defer func() {
		_ = r.Close()
	}()

	if _, err = io.ReadAll(r); err != nil {
		return errors.Wrap(err, "")
	}

	return nil
}

func ReadContainerLogs(
	ctx context.Context,
	client *dockerClient.Client,
	id string,
	options dockerTypes.ContainerLogsOptions,
	callback func(uint8, []byte),
) error {
	options.Timestamps = true

	var timestamp string
	for {
		options.Since = timestamp

		reader, err := client.ContainerLogs(ctx, id, options)
		if err != nil {
			return err
		}

		if t, err := readContainerLogs(ctx, reader, callback); err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return nil
			case errors.Is(err, errContainerLogIgnore):
				<-time.After(time.Millisecond * 600)

				timestamp = t

				continue
			case errors.Is(err, io.EOF):
				return nil
			default:
				timestamp = t

				continue
			}
		}
	}
}

func readContainerLogs(ctx context.Context, reader io.Reader, callback func(uint8, []byte)) (string, error) {
	var timestamp, msg string
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			h := make([]byte, 8)
			if _, err := reader.Read(h); err != nil {
				return timestamp, err
			}

			count := binary.BigEndian.Uint32(h[4:])
			l := make([]byte, count)
			if _, err := reader.Read(l); err != nil {
				if bytes.Contains(l, []byte("Error grabbing logs")) {
					_, _ = fmt.Fprintf(os.Stderr, "grabbing error: %q\n", string(l))

					return timestamp, errContainerLogIgnore.Errorf("%s: %w", l, err)
				}

				return timestamp, errors.Wrapf(err, "failed to read logs body, %q", string(l))
			}

			s := strings.SplitN(string(l[:len(l)-1]), " ", 2)
			timestamp, msg = s[0], s[1]

			callback(h[0], []byte(msg))
		}
	}
}

func ContainerName(alias string) string {
	return fmt.Sprintf("%s-%s", ContainerLabel, alias)
}
