package api

import (
	"net/http"
	"runtime"
)

// metaDocument is the build identity the SPA and the operator see.
type metaDocument struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuiltAt       string `json:"builtAt"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	SchemaVersion int    `json:"schemaVersion"`
}

// handleGetMeta reports which binary is running, so a support question starts
// with facts instead of a guess.
func (s *Server) handleGetMeta(w http.ResponseWriter, r *http.Request) {
	version := s.build.Version
	if version == "" {
		version = "dev"
	}
	commit := s.build.Commit
	if commit == "" {
		commit = "none"
	}
	builtAt := s.build.BuiltAt
	if builtAt == "" {
		builtAt = "unknown"
	}
	osName := s.build.OS
	if osName == "" {
		osName = runtime.GOOS
	}
	arch := s.build.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	schemaVersion := s.build.SchemaVersion
	if schemaVersion == 0 && s.config != nil {
		schemaVersion = s.config.SchemaVersion
	}

	s.writeJSON(w, http.StatusOK, metaDocument{
		Version:       version,
		Commit:        commit,
		BuiltAt:       builtAt,
		OS:            osName,
		Arch:          arch,
		SchemaVersion: schemaVersion,
	})
}
