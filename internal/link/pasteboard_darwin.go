package link

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit
#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <string.h>

static void *copyData(NSData *d, size_t *len) {
	*len = d.length;
	void *buf = malloc(d.length > 0 ? d.length : 1);
	memcpy(buf, d.bytes, d.length);
	return buf;
}

// Screenshots carry PNG; other image sources often only TIFF, which is converted.
static NSData *pngData(void) {
	NSPasteboard *pb = [NSPasteboard generalPasteboard];
	NSData *png = [pb dataForType:NSPasteboardTypePNG];
	if (png != nil) {
		return png;
	}
	NSData *tiff = [pb dataForType:NSPasteboardTypeTIFF];
	if (tiff == nil) {
		return nil;
	}
	NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:tiff];
	return [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
}

static int hasImage(void) {
	@autoreleasepool {
		NSArray *types = [[NSPasteboard generalPasteboard] types];
		return [types containsObject:NSPasteboardTypePNG] || [types containsObject:NSPasteboardTypeTIFF];
	}
}

static int hasText(void) {
	@autoreleasepool {
		return [[[NSPasteboard generalPasteboard] types] containsObject:NSPasteboardTypeString];
	}
}

// Returns NULL when there is no image, or when a TIFF could not be converted (*len = 1).
static void *readPNG(size_t *len) {
	@autoreleasepool {
		*len = 0;
		NSData *d = pngData();
		if (d == nil) {
			*len = hasImage() ? 1 : 0;
			return NULL;
		}
		return copyData(d, len);
	}
}

static char *readText(size_t *len) {
	@autoreleasepool {
		NSString *s = [[NSPasteboard generalPasteboard] stringForType:NSPasteboardTypeString];
		if (s == nil) {
			*len = 0;
			return NULL;
		}
		return copyData([s dataUsingEncoding:NSUTF8StringEncoding], len);
	}
}

static int writeData(const void *buf, size_t len, int png) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		[pb clearContents];
		NSData *d = [NSData dataWithBytes:buf length:len];
		if (png) {
			return [pb setData:d forType:NSPasteboardTypePNG] ? 1 : 0;
		}
		NSString *s = [[NSString alloc] initWithData:d encoding:NSUTF8StringEncoding];
		if (s == nil) {
			return 0;
		}
		return [pb setString:s forType:NSPasteboardTypeString] ? 1 : 0;
	}
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// MacPasteboard is the general NSPasteboard.
type MacPasteboard struct{}

func (MacPasteboard) Types() ([]string, error) {
	var out []string
	if C.hasImage() != 0 {
		out = append(out, MIMEPNG)
	}
	if C.hasText() != 0 {
		out = append(out, MIMEText)
	}
	return out, nil
}

func (MacPasteboard) PNG() ([]byte, error) {
	var n C.size_t
	buf := C.readPNG(&n)
	if buf == nil {
		if n != 0 {
			return nil, errors.New("clipboard image could not be converted to PNG")
		}
		return nil, nil
	}
	defer C.free(buf)
	return C.GoBytes(buf, C.int(n)), nil
}

func (MacPasteboard) Text() (string, error) {
	var n C.size_t
	buf := C.readText(&n)
	if buf == nil {
		return "", errors.New("clipboard holds no text")
	}
	defer C.free(unsafe.Pointer(buf))
	return C.GoStringN(buf, C.int(n)), nil
}

func write(data []byte, png bool) error {
	flag := C.int(0)
	if png {
		flag = 1
	}
	var ptr unsafe.Pointer
	if len(data) > 0 {
		ptr = C.CBytes(data)
		defer C.free(ptr)
	}
	if C.writeData(ptr, C.size_t(len(data)), flag) == 0 {
		return errors.New("writing to the clipboard failed")
	}
	return nil
}

func (MacPasteboard) SetText(s string) error { return write([]byte(s), false) }
func (MacPasteboard) SetPNG(b []byte) error  { return write(b, true) }
