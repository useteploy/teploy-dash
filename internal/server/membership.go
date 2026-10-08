package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// membershipServer binds explicit assignments to a registered stable identity.
func (s *Server) membershipServer(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	for _, srv := range s.serversBestEffort() {
		if srv.Name == name {
			return serverEnvelopeID(srv), nil
		}
	}
	return "", fmt.Errorf("server %q is not configured", name)
}

func removeMembership(apps *[]string, refs *[]groupAppRef, app, serverID string, legacy bool) error {
	count := 0
	for _, ref := range *refs {
		if ref.App == app {
			count++
		}
	}
	bare := false
	for _, name := range *apps {
		if name == app {
			bare = true
		}
	}
	if serverID == "" && !legacy && count > 0 {
		if count > 1 || bare {
			ids := []string{}
			for _, ref := range *refs {
				if ref.App == app {
					ids = append(ids, ref.ServerID)
				}
			}
			return fmt.Errorf("%w (%s); specify server_id or legacy=1", errGroupAppAmbiguous, strings.Join(ids, ", "))
		}
		for _, ref := range *refs {
			if ref.App == app {
				serverID = ref.ServerID
				break
			}
		}
	}
	if serverID != "" {
		kept := make([]groupAppRef, 0, len(*refs))
		for _, ref := range *refs {
			if ref.App != app || ref.ServerID != serverID {
				kept = append(kept, ref)
			}
		}
		*refs = kept
	} else {
		kept := make([]string, 0, len(*apps))
		for _, name := range *apps {
			if name != app {
				kept = append(kept, name)
			}
		}
		*apps = kept
	}
	return nil
}

func addMembership(apps *[]string, refs *[]groupAppRef, app, serverID string) bool {
	if serverID != "" {
		for _, ref := range *refs {
			if ref.App == app && ref.ServerID == serverID {
				return false
			}
		}
		*refs = append(*refs, groupAppRef{ServerID: serverID, App: app})
	} else {
		for _, name := range *apps {
			if name == app {
				return false
			}
		}
		*apps = append(*apps, app)
	}
	return true
}

func (s *Server) removeGroupMembership(w http.ResponseWriter, r *http.Request, group, project, app string) {
	serverID := r.URL.Query().Get("server_id")
	legacy := r.URL.Query().Get("legacy") == "1"
	if legacy && serverID != "" {
		writeError(w, "choose server_id or legacy, not both")
		return
	}
	_, err := updateGroups(func(data *groupData) error {
		for i := range data.Groups {
			g := &data.Groups[i]
			if g.Name != group {
				continue
			}
			if project == "" {
				return removeMembership(&g.Apps, &g.ServerApps, app, serverID, legacy)
			}
			for j := range g.Projects {
				p := &g.Projects[j]
				if p.Name == project {
					return removeMembership(&p.Apps, &p.ServerApps, app, serverID, legacy)
				}
			}
		}
		return errGroupNotFound
	})
	if errors.Is(err, errGroupAppAmbiguous) {
		writeErrorStatus(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeData(w, map[string]string{"status": "unassigned"})
}

func (s *Server) assignProjectMembership(w http.ResponseWriter, r *http.Request, group, project string) {
	var body struct {
		App    string `json:"app"`
		Server string `json:"server"`
	}
	if err := strictDecode(r, &body); err != nil || strings.TrimSpace(body.App) == "" {
		writeError(w, "app is required")
		return
	}
	id, err := s.membershipServer(body.Server)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusNotFound)
		return
	}
	_, err = updateGroups(func(data *groupData) error {
		for i := range data.Groups {
			g := &data.Groups[i]
			if g.Name != group {
				continue
			}
			for j := range g.Projects {
				p := &g.Projects[j]
				if p.Name != project {
					continue
				}
				addMembership(&p.Apps, &p.ServerApps, body.App, id)
				addMembership(&g.Apps, &g.ServerApps, body.App, id)
				return nil
			}
		}
		return errGroupNotFound
	})
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeData(w, map[string]string{"status": "assigned"})
}
