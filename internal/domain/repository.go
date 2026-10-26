package domain

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
