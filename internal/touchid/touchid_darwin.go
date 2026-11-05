// Package touchid asks the user at the laptop to authenticate, with Touch ID
// or their password, through macOS LocalAuthentication.
package touchid

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework LocalAuthentication
#import <Foundation/Foundation.h>
#import <LocalAuthentication/LocalAuthentication.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

static void *newContext(void) {
	return (__bridge_retained void *)[[LAContext alloc] init];
}

static void releaseContext(void *c) {
	CFBridgingRelease(c);
}

// Dismisses the sheet of an evaluation in progress, which then fails with LAErrorAppCancel.
static void cancelContext(void *c) {
	[(__bridge LAContext *)c invalidate];
}

// Shows the authentication sheet with reason and waits for the user. Returns true only once
// they authenticated; else sets *code and *err, which the caller frees, to the LAError.
static bool evaluate(void *c, const char *reason, long *code, char **err) {
	@autoreleasepool {
		LAContext *context = (__bridge LAContext *)c;
		NSError *e = nil;
		if (![context canEvaluatePolicy:LAPolicyDeviceOwnerAuthentication error:&e]) {
			*code = e.code;
			*err = strdup(e == nil ? "LocalAuthentication cannot ask, and gave no reason" : e.localizedDescription.UTF8String);
			return false;
		}
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		__block BOOL succeeded = NO;
		__block NSError *failure = nil;
		[context evaluatePolicy:LAPolicyDeviceOwnerAuthentication
		        localizedReason:[NSString stringWithUTF8String:reason]
		                  reply:^(BOOL success, NSError *error) {
			succeeded = success;
			failure = error;
			dispatch_semaphore_signal(done);
		}];
		dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
		if (succeeded) {
			return true;
		}
		*code = failure.code;
		*err = strdup(failure == nil ? "LocalAuthentication gave no reason" : failure.localizedDescription.UTF8String);
		return false;
	}
}
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"
)

// Error is why LocalAuthentication did not authenticate the user: they
// cancelled or failed, or it could not ask, as when no one is logged in at
// the screen.
type Error struct {
	// Code is the LAError code.
	Code int
	Text string
}

func (e Error) Error() string { return fmt.Sprintf("%s (LocalAuthentication error %d)", e.Text, e.Code) }

// Ask shows the system's authentication sheet, which says the process "is
// trying to" reason, and returns nil once the user authenticated. ctx ending
// dismisses the sheet; Ask then returns ctx's error.
func Ask(ctx context.Context, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	la := C.newContext()
	defer C.releaseContext(la)
	creason := C.CString(reason)
	defer C.free(unsafe.Pointer(creason))
	done := make(chan error, 1)
	go func() {
		var code C.long
		var cerr *C.char
		if C.evaluate(la, creason, &code, &cerr) {
			done <- nil
			return
		}
		defer C.free(unsafe.Pointer(cerr))
		done <- Error{Code: int(code), Text: C.GoString(cerr)}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		C.cancelContext(la)
		<-done
		return ctx.Err()
	}
}
