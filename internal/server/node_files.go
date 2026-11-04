package server

import "github.com/naozhi/naozhi/internal/upstream"

// NodeFiles serves this node's files to a primary over the reverse link
// (planner RFC §15.5 ④): the same listing, reads, batch stats and outputs
// the dashboard's own file API serves, under this node's roots and rules.
func (s *Server) NodeFiles() upstream.Files {
	return upstream.Files{
		List:    s.projectFiles.ListFilesFor,
		Read:    s.projectFiles.ReadFileFor,
		Exists:  s.projectFiles.FilesExistFor,
		Outputs: s.sessionH.OutputsFor,
	}
}
