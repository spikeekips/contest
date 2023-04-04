package contest

import (
	"fmt"

	"github.com/spikeekips/mitum/util"
)

type BuildInfo struct {
	util.BuildInfo
	MitumBranch  string
	MitumCommit  string
	MitumVersion util.Version
}

func ParseBuildInfo(
	version,
	branch,
	commit,
	mitumVersion,
	mitumBranch,
	mitumCommit,
	buildTime string,
) (BuildInfo, error) {
	bi := BuildInfo{}

	switch bbi, err := util.ParseBuildInfo(version, branch, commit, buildTime); {
	case err != nil:
		return bi, err
	default:
		bi.BuildInfo = bbi
	}

	switch i, err := util.ParseVersion(mitumVersion); {
	case err != nil:
		return bi, err
	default:
		bi.MitumVersion = i
	}

	bi.MitumBranch = mitumBranch
	bi.MitumCommit = mitumCommit

	return bi, nil
}

func (bi BuildInfo) String() string {
	return fmt.Sprintf(`* mitum build info
        version: %s
         branch: %s
         commit: %s
  mitum version: %s
   mitum branch: %s
   mitum commit: %s
          build: %s`,
		bi.Version, bi.Branch, bi.Commit,
		bi.MitumVersion, bi.MitumBranch, bi.MitumCommit,
		bi.BuildTime,
	)
}
