package branch

import (
	"regexp"
	"strings"
)

var SlotFromBranchRE = regexp.MustCompile(`^(slot-.+)$`)

func BranchToSlot(branch string) (slotName string) {
	if SlotFromBranchRE.MatchString(branch) {
		matches := SlotFromBranchRE.FindStringSubmatch(branch)
		if len(matches) > 0 {
			slotName = matches[1]
		}
	}
	return slotName
}

func RefToBranch(ref string) (branch string) {
	if strings.HasPrefix(ref, "refs/heads/") {
		branch = strings.Replace(ref, "refs/heads/", "", 1)
	}
	return branch
}

func RepoToService(repo string) (runEnv string, svcName string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = r.(error)
		}
	}()

	// /dev-mm/tsa -> dev-mm = env, tsa = service
	runEnv = strings.SplitN(repo, "/", 3)[1]
	svcName = strings.SplitN(repo, "/", 3)[2]

	return runEnv, svcName, nil
}
