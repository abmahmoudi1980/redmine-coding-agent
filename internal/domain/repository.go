package domain

import "path/filepath"

// RepositoryMapping is one configured mapping from a Redmine project to a git
// repository. It is the only source of truth for resolution (FR-014).
type RepositoryMapping struct {
	ID                  int64
	RedmineProject      string
	RepositoryPath      string
	DisplayName         string
	BaseBranch          string
	IsDefault           bool
	PublishEnabled      bool
	MergeRequestEnabled bool
	Enabled             bool
	Checks              []CheckDefinition
}

// IsLocal reports whether the mapping points at a repository on this machine
// instead of a GitLab project. A local mapping needs no GitLab server: the
// pipeline clones from the path and can push the dedicated branch back to it.
func (r RepositoryMapping) IsLocal() bool { return IsLocalRepositoryPath(r.RepositoryPath) }

// IsLocalRepositoryPath reports whether a configured repository path is an
// absolute filesystem path. GitLab project paths (group/project) are relative by
// construction, so absoluteness is an unambiguous discriminator on every OS.
func IsLocalRepositoryPath(path string) bool {
	path = filepath.Clean(path)
	return path != "" && path != "." && filepath.IsAbs(path)
}

// CheckDefinition is one configured validation command for a repository.
type CheckDefinition struct {
	ID             int64
	Name           string
	CommandLinux   string
	CommandWindows string
	Required       bool
	TimeoutSeconds int
	Position       int
}

// CommandFor returns the command rendered for the given GOOS.
func (c CheckDefinition) CommandFor(goos string) string {
	if goos == "windows" && c.CommandWindows != "" {
		return c.CommandWindows
	}
	return c.CommandLinux
}
