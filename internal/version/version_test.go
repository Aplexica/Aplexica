package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestStringContainsVersion(t *testing.T) {
	s := String()
	if !strings.Contains(s, Version) {
		t.Errorf("String()=%q must contain Version=%q", s, Version)
	}
	if !strings.Contains(s, "aplexica") {
		t.Errorf("String()=%q must contain the binary name", s)
	}
}

func TestStringDoesNotAppendTextAfterVersion(t *testing.T) {
	if got, want := String(), "aplexica "+Version; got != want {
		t.Fatalf("String()=%q, want exact numeric release identity %q", got, want)
	}
}

func resetVersionState(t *testing.T) {
	t.Helper()
	origCommit := GitCommit
	origDate := BuildDate
	origModified := Modified
	origReadBuildInfo := ReadBuildInfo

	t.Cleanup(func() {
		GitCommit = origCommit
		BuildDate = origDate
		Modified = origModified
		ReadBuildInfo = origReadBuildInfo
	})

	GitCommit = "unknown"
	BuildDate = "unknown"
	Modified = false
}

func TestBuildInfoFallback(t *testing.T) {
	resetVersionState(t)

	ReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
				{Key: "vcs.time", Value: "2026-09-17T05:00:00Z"},
			},
		}, true
	}

	initBuildInfo()

	if GitCommit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("GitCommit = %q, want vcs.revision fallback value", GitCommit)
	}
	if BuildDate != "2026-09-17T05:00:00Z" {
		t.Fatalf("BuildDate = %q, want vcs.time fallback value", BuildDate)
	}
	if Modified {
		t.Fatalf("Modified = true, want false when vcs.modified is absent")
	}
}

func TestLdflagsPrecedence(t *testing.T) {
	resetVersionState(t)

	GitCommit = "ldflag-commit-sha"
	BuildDate = "2026-01-01T00:00:00Z"

	ReadBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "vcs-revision-sha"},
				{Key: "vcs.time", Value: "2026-09-17T05:00:00Z"},
			},
		}, true
	}

	initBuildInfo()

	if GitCommit != "ldflag-commit-sha" {
		t.Fatalf("GitCommit = %q, want ldflag value to take precedence", GitCommit)
	}
	if BuildDate != "2026-01-01T00:00:00Z" {
		t.Fatalf("BuildDate = %q, want ldflag value to take precedence", BuildDate)
	}
}

func TestIndividualUnknownFieldsPopulatedIndependently(t *testing.T) {
	t.Run("commit populated when date is stamped", func(t *testing.T) {
		resetVersionState(t)
		BuildDate = "2026-01-01T00:00:00Z"

		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "vcs-revision-sha"},
					{Key: "vcs.time", Value: "2026-09-17T05:00:00Z"},
				},
			}, true
		}

		initBuildInfo()

		if GitCommit != "vcs-revision-sha" {
			t.Fatalf("GitCommit = %q, want populated from vcs.revision", GitCommit)
		}
		if BuildDate != "2026-01-01T00:00:00Z" {
			t.Fatalf("BuildDate = %q, want ldflag value preserved", BuildDate)
		}
	})

	t.Run("date populated when commit is stamped", func(t *testing.T) {
		resetVersionState(t)
		GitCommit = "ldflag-commit-sha"

		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "vcs-revision-sha"},
					{Key: "vcs.time", Value: "2026-09-17T05:00:00Z"},
				},
			}, true
		}

		initBuildInfo()

		if GitCommit != "ldflag-commit-sha" {
			t.Fatalf("GitCommit = %q, want ldflag value preserved", GitCommit)
		}
		if BuildDate != "2026-09-17T05:00:00Z" {
			t.Fatalf("BuildDate = %q, want populated from vcs.time", BuildDate)
		}
	})
}

func TestMissingVCSMetadataPreservesDefaults(t *testing.T) {
	t.Run("read build info returns false", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return nil, false
		}

		initBuildInfo()

		if GitCommit != "unknown" || BuildDate != "unknown" || Modified {
			t.Fatalf("unexpected state: GitCommit=%q BuildDate=%q Modified=%v", GitCommit, BuildDate, Modified)
		}
	})

	t.Run("read build info returns nil info", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return nil, true
		}

		initBuildInfo()

		if GitCommit != "unknown" || BuildDate != "unknown" || Modified {
			t.Fatalf("unexpected state: GitCommit=%q BuildDate=%q Modified=%v", GitCommit, BuildDate, Modified)
		}
	})

	t.Run("settings without vcs metadata", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "-compiler", Value: "gc"},
				},
			}, true
		}

		initBuildInfo()

		if GitCommit != "unknown" || BuildDate != "unknown" || Modified {
			t.Fatalf("unexpected state: GitCommit=%q BuildDate=%q Modified=%v", GitCommit, BuildDate, Modified)
		}
	})

	t.Run("empty vcs settings values preserve defaults", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: ""},
					{Key: "vcs.time", Value: ""},
				},
			}, true
		}

		initBuildInfo()

		if GitCommit != "unknown" || BuildDate != "unknown" || Modified {
			t.Fatalf("unexpected state: GitCommit=%q BuildDate=%q Modified=%v", GitCommit, BuildDate, Modified)
		}
	})
}

func TestModifiedState(t *testing.T) {
	t.Run("vcs.modified is true", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef"},
					{Key: "vcs.modified", Value: "true"},
				},
			}, true
		}

		initBuildInfo()

		if !Modified {
			t.Fatalf("Modified = false, want true")
		}
		if GitCommit != "0123456789abcdef" {
			t.Fatalf("GitCommit = %q, want raw revision without dirty suffix", GitCommit)
		}
	})

	t.Run("vcs.modified is false", func(t *testing.T) {
		resetVersionState(t)
		ReadBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef"},
					{Key: "vcs.modified", Value: "false"},
				},
			}, true
		}

		initBuildInfo()

		if Modified {
			t.Fatalf("Modified = true, want false")
		}
		if GitCommit != "0123456789abcdef" {
			t.Fatalf("GitCommit = %q, want raw revision", GitCommit)
		}
	})
}

func TestStringOutputPreservedWithBuildInfo(t *testing.T) {
	resetVersionState(t)
	GitCommit = "0123456789abcdef"
	BuildDate = "2026-09-17T05:00:00Z"
	Modified = true

	if got, want := String(), "aplexica "+Version; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
