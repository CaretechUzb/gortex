package daemon

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testOverlaySnapshotMaxFiles = 257
	testOverlaySnapshotMaxBytes = 4_259_840
)

func TestOverlayManagerSnapshotForBoundedFileBoundary(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")
	for i := 0; i < testOverlaySnapshotMaxFiles; i++ {
		require.NoError(t, m.Push(id, OverlayFile{
			Path:    fmt.Sprintf("file-%03d.go", i),
			Content: "package p",
		}, nil))
	}

	workspace, files, err := m.SnapshotForBounded(
		id, testOverlaySnapshotMaxFiles, testOverlaySnapshotMaxBytes,
	)
	require.NoError(t, err)
	require.Equal(t, "ws", workspace)
	require.Len(t, files, testOverlaySnapshotMaxFiles)
	require.Equal(t, "file-000.go", files[0].Path)
	require.Equal(t, "file-256.go", files[len(files)-1].Path)

	require.NoError(t, m.Push(id, OverlayFile{Path: "file-257.go", Content: "package p"}, nil))
	workspace, files, err = m.SnapshotForBounded(
		id, testOverlaySnapshotMaxFiles, testOverlaySnapshotMaxBytes,
	)
	var tooLarge *ErrOverlaySnapshotTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, "files", tooLarge.Resource)
	require.Equal(t, testOverlaySnapshotMaxFiles, tooLarge.Limit)
	require.Empty(t, workspace, "rejected snapshots must not return partial metadata")
	require.Nil(t, files, "rejected snapshots must not return a partial slice")
}

func TestOverlayManagerSnapshotForBoundedCountsAllStringFields(t *testing.T) {
	t.Run("exact combined boundary", func(t *testing.T) {
		m := NewOverlayManager(time.Minute)
		id := m.Register("ws")
		require.NoError(t, m.Push(id, OverlayFile{
			Path:    "path",
			Content: "abc",
			BaseSHA: "sha",
		}, nil))

		_, files, err := m.SnapshotForBounded(id, 1, 10)
		require.NoError(t, err)
		require.Len(t, files, 1)

		require.NoError(t, m.Push(id, OverlayFile{
			Path:    "path",
			Content: "abcd",
			BaseSHA: "sha",
		}, nil))
		workspace, files, err := m.SnapshotForBounded(id, 1, 10)
		var tooLarge *ErrOverlaySnapshotTooLarge
		require.ErrorAs(t, err, &tooLarge)
		require.Equal(t, "bytes", tooLarge.Resource)
		require.Empty(t, workspace)
		require.Nil(t, files)
	})

	t.Run("path exact and plus one", func(t *testing.T) {
		m := NewOverlayManager(time.Minute)
		id := m.Register("ws")
		require.NoError(t, m.Push(id, OverlayFile{Path: "1234"}, nil))
		_, files, err := m.SnapshotForBounded(id, 1, 4)
		require.NoError(t, err)
		require.Len(t, files, 1)

		require.NoError(t, m.Push(id, OverlayFile{Path: "12345"}, nil))
		_, files, err = m.SnapshotForBounded(id, 2, 4)
		var tooLarge *ErrOverlaySnapshotTooLarge
		require.ErrorAs(t, err, &tooLarge)
		require.Equal(t, "bytes", tooLarge.Resource)
		require.Nil(t, files)
	})

	t.Run("base sha exact and plus one", func(t *testing.T) {
		m := NewOverlayManager(time.Minute)
		id := m.Register("ws")
		require.NoError(t, m.Push(id, OverlayFile{Path: "p", BaseSHA: "1234"}, nil))
		_, files, err := m.SnapshotForBounded(id, 1, 5)
		require.NoError(t, err)
		require.Len(t, files, 1)

		require.NoError(t, m.Push(id, OverlayFile{Path: "p", BaseSHA: "12345"}, nil))
		_, files, err = m.SnapshotForBounded(id, 1, 5)
		var tooLarge *ErrOverlaySnapshotTooLarge
		require.ErrorAs(t, err, &tooLarge)
		require.Equal(t, "bytes", tooLarge.Resource)
		require.Nil(t, files)
	})
}

func TestOverlayManagerBoundedSnapshotsRequirePositiveLimits(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")

	for _, limits := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		_, files, err := m.SnapshotForBounded(id, limits[0], limits[1])
		require.Error(t, err)
		require.Nil(t, files)

		files, err = m.FilesForBranchBounded(id, MainBranchName, limits[0], limits[1])
		require.Error(t, err)
		require.Nil(t, files)
	}
}

func TestOverlayManagerBoundedRejectionBumpsLastUsed(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")
	require.NoError(t, m.Push(id, OverlayFile{Path: "a.go", Content: "a"}, nil))

	old := time.Unix(1, 0)
	m.mu.Lock()
	m.sessions[id].LastUsed = old
	m.mu.Unlock()

	_, _, err := m.SnapshotForBounded(id, 1, 1)
	var tooLarge *ErrOverlaySnapshotTooLarge
	require.ErrorAs(t, err, &tooLarge)
	status, statusErr := m.StatusFor(id)
	require.NoError(t, statusErr)
	require.True(t, status.LastUsed.After(old))

	m.mu.Lock()
	m.sessions[id].LastUsed = old
	m.mu.Unlock()
	files, err := m.FilesForBranchBounded(id, MainBranchName, 1, 1)
	require.ErrorAs(t, err, &tooLarge)
	require.Nil(t, files)
	status, statusErr = m.StatusFor(id)
	require.NoError(t, statusErr)
	require.True(t, status.LastUsed.After(old))
}

func TestOverlayManagerBoundedMissingUsesSharedLock(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	m.mu.RLock()
	defer m.mu.RUnlock()

	tests := map[string]func() error{
		"active": func() error {
			_, _, err := m.SnapshotForBounded("ordinary-mcp-session", 1, 1)
			return err
		},
		"named branch": func() error {
			_, err := m.FilesForBranchBounded("ordinary-mcp-session", MainBranchName, 1, 1)
			return err
		},
	}
	for name, call := range tests {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			require.ErrorIs(t, err, ErrSessionNotFound, name)
		case <-time.After(time.Second):
			t.Fatalf("%s snapshot waited for an exclusive manager lock", name)
		}
	}
}

func TestOverlayManagerSnapshotForBoundedConcurrentCoherent(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")
	require.NoError(t, m.Push(id, OverlayFile{Path: "state.go", Content: "main"}, nil))
	_, err := m.Fork(id, ForkOptions{Name: "alternate"})
	require.NoError(t, err)
	require.NoError(t, m.PushToBranch(id, "alternate", OverlayFile{
		Path: "state.go", Content: "alternate",
	}, nil))

	const iterations = 500
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			branch := MainBranchName
			if i%2 == 0 {
				branch = "alternate"
			}
			if switchErr := m.SwitchBranch(id, branch); switchErr != nil {
				errs <- switchErr
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			workspace, files, snapshotErr := m.SnapshotForBounded(id, 1, 64)
			if snapshotErr != nil {
				errs <- snapshotErr
				return
			}
			if workspace != "ws" || len(files) != 1 || files[0].Path != "state.go" ||
				(files[0].Content != "main" && files[0].Content != "alternate") {
				errs <- fmt.Errorf("incoherent snapshot: workspace=%q files=%+v", workspace, files)
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestOverlayManagerFilesForBranchBoundedSortsAndDoesNotAlias(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")
	_, err := m.Fork(id, ForkOptions{Name: "candidate"})
	require.NoError(t, err)
	require.NoError(t, m.PushToBranch(id, "candidate", OverlayFile{Path: "b.go", Content: "b"}, nil))
	require.NoError(t, m.PushToBranch(id, "candidate", OverlayFile{Path: "a.go", Content: "a"}, nil))

	files, err := m.FilesForBranchBounded(id, "candidate", 2, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"a.go", "b.go"}, []string{files[0].Path, files[1].Path})
	files[0].Content = strings.Repeat("x", 3)

	again, err := m.FilesForBranchBounded(id, "candidate", 2, 10)
	require.NoError(t, err)
	require.Equal(t, "a", again[0].Content)

	rejected, err := m.FilesForBranchBounded(id, "candidate", 1, 10)
	var tooLarge *ErrOverlaySnapshotTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Nil(t, rejected)
}
