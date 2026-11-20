package video2dvd

// Test-only exports for the VOB multiplexer. They live apart from
// export_test.go so that unrelated shims do not collide.
import (
	"context"
	"io"
)

type TestDVDVOBUSearchEntry = dvdVOBUSearchEntry
type TestDVDVOBUSearchInfo = dvdVOBUSearchInfo

var (
	ExportDVDVOBUSearchInfo  = buildDVDVOBUSearchInfo
	ExportDVDVideoFieldTicks = dvdVideoFieldTicks
)

func ExportMuxDVDTitleVOB(ctx context.Context, title EncodedTitle, workDir string, dst io.Writer, record func(TestDVDVOBURecord) error, buttonPages [][]DVDNAVButton) error {
	return muxDVDTitleVOB(ctx, title, workDir, dst, record, buttonPages)
}
