package contest

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerClient "github.com/docker/docker/client"
	"github.com/pkg/errors"
)

var ContainerLabel = "mitum-contest"

func ExistsImage(client *dockerClient.Client, img string) (bool, error) {
	i, err := client.ImageList(
		context.Background(),
		image.ListOptions{
			All: true,
			Filters: filters.NewArgs(filters.KeyValuePair{
				Key:   "reference",
				Value: img,
			}),
		},
	)
	if err != nil {
		return false, errors.WithStack(err)
	}

	return len(i) > 0, nil
}

func PullImage(client *dockerClient.Client, img string) error {
	r, err := client.ImagePull(
		context.Background(),
		img,
		image.PullOptions{},
	)
	if err != nil {
		return errors.WithStack(err)
	}

	defer func() {
		_ = r.Close()
	}()

	if _, err = io.ReadAll(r); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
