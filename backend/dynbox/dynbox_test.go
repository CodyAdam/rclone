// Test Box filesystem interface
package dynbox_test

import (
	"testing"

	"github.com/rclone/rclone/backend/dynbox"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestDynbox:",
		NilObject:  (*dynbox.Object)(nil),
	})
}
