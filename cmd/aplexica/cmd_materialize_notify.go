package main

import (
	"fmt"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/spf13/cobra"
)

func notifyDaemonMaterializeConversation(cmd *cobra.Command, artifactID, agent, branch string) {
	sockPath, err := daemonControlSocket()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: branch pointer saved, but daemon materialization was not requested: %v\n", err)
		return
	}
	resp, err := daemon.SendCommand(sockPath, daemon.Request{
		Command:    "materialize",
		ArtifactID: artifactID,
		Agent:      agent,
		Branch:     branch,
	})
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: branch pointer saved, but daemon is not reachable for immediate materialization: %v\n", err)
		return
	}
	if !resp.OK {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: branch pointer saved, but immediate materialization failed: %s\n", resp.Error)
		return
	}
	data, _ := resp.Data.(map[string]any)
	path, _ := data["path"].(string)
	materialized, _ := data["materialized"].(bool)
	if materialized && path != "" {
		fmt.Fprintf(cmd.OutOrStdout(),
			"materialized branch %q for agent %q at %s\n", branch, agent, path)
	}
}
