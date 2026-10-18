package api

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Omitted options preserve the legacy enabled/all-project creation contract.
// Explicit null must not silently enable or widen a channel.
func channelCreationOptions(enabledRaw, projectsRaw json.RawMessage) (bool, []string, error) {
	enabled := true
	if len(enabledRaw) != 0 {
		switch string(bytes.TrimSpace(enabledRaw)) {
		case "true":
		case "false":
			enabled = false
		default:
			return false, nil, errors.New("enabled must be a boolean")
		}
	}
	projectIDs := []string{}
	if len(projectsRaw) != 0 {
		if bytes.Equal(bytes.TrimSpace(projectsRaw), []byte("null")) || json.Unmarshal(projectsRaw, &projectIDs) != nil {
			return false, nil, errors.New("project_ids must be an array of project identifiers")
		}
	}
	projectIDs, err := normalizeChannelProjectIDs(projectIDs)
	return enabled, projectIDs, err
}

func normalizeChannelProjectIDs(input []string) ([]string, error) {
	projectIDs := make([]string, 0, min(len(input), maxChannelProjects))
	seen := make(map[string]struct{}, min(len(input), maxChannelProjects))
	for _, projectID := range input {
		if len(projectID) == 0 || len(projectID) > 128 {
			return nil, errors.New("invalid project identifier in project_ids")
		}
		for _, c := range projectID {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return nil, errors.New("invalid project identifier in project_ids")
			}
		}
		if _, duplicate := seen[projectID]; duplicate {
			continue
		}
		if len(projectIDs) == maxChannelProjects {
			return nil, errors.New("too many project_ids (max 256)")
		}
		seen[projectID] = struct{}{}
		projectIDs = append(projectIDs, projectID)
	}
	return projectIDs, nil
}
