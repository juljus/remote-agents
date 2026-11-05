package laptopfiles

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>
#include <stdlib.h>
#include <string.h>

// Moves path to the Trash; returns where it went, or NULL with *err set. The caller frees either.
static char *trashItem(const char *path, char **err) {
	@autoreleasepool {
		NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
		NSURL *result = nil;
		NSError *e = nil;
		if (![[NSFileManager defaultManager] trashItemAtURL:url resultingItemURL:&result error:&e]) {
			*err = strdup(e.localizedDescription.UTF8String);
			return NULL;
		}
		return strdup(result.path.UTF8String);
	}
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Trash moves path to the user's Trash, as Finder's Move to Trash does, and
// says where it went.
func Trash(path string) (string, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var cerr *C.char
	trashed := C.trashItem(cpath, &cerr)
	if trashed == nil {
		defer C.free(unsafe.Pointer(cerr))
		return "", errors.New(C.GoString(cerr))
	}
	defer C.free(unsafe.Pointer(trashed))
	return C.GoString(trashed), nil
}
