package laptopfiles

/*
#include <fcntl.h>
#include <sys/param.h>
#include <unistd.h>
#include <errno.h>
#include <stdlib.h>

// Writes the path the file system holds for path, in its own case, into buf of MAXPATHLEN bytes.
static int diskPath(const char *path, char *buf) {
	int fd = open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK);
	if (fd < 0) {
		return errno;
	}
	int rc = fcntl(fd, F_GETPATH, buf);
	int err = errno;
	close(fd);
	return rc < 0 ? err : 0;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// diskPath is path as the file system spells it. APFS ignores case, so a
// path typed as ~/library/x names ~/Library/x; checks on the spelling need
// the file system's.
func diskPath(path string) (string, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	buf := make([]byte, C.MAXPATHLEN)
	if errno := C.diskPath(cpath, (*C.char)(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", fmt.Errorf("finding how %s is spelled on disk: %w", path, syscall.Errno(errno))
	}
	return C.GoString((*C.char)(unsafe.Pointer(&buf[0]))), nil
}
