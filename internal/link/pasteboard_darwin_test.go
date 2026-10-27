package link

import "testing"

// Read-only: the test must not clobber the user's clipboard.
func TestMacPasteboardReads(t *testing.T) {
	var pb MacPasteboard
	types, err := pb.Types()
	if err != nil {
		t.Fatalf("Types: %v", err)
	}
	for _, typ := range types {
		switch typ {
		case MIMEPNG:
			data, err := pb.PNG()
			if err != nil || len(data) < 8 || string(data[1:4]) != "PNG" {
				t.Fatalf("PNG = %d bytes, %v", len(data), err)
			}
		case MIMEText:
			if _, err := pb.Text(); err != nil {
				t.Fatalf("Text: %v", err)
			}
		}
	}
	t.Logf("clipboard types: %v", types)
}
