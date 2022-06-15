package contest

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	dockerClient "github.com/docker/docker/client"
	"github.com/pkg/errors"
)

var ContainerLabel = "mitum-contest"

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
