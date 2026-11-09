//go:build linux && cgo

package main

// A build with cgo starts threads Go's AllThreadsSyscall cannot reach, so
// the namespace child cannot clear its ambient capabilities from Go. It
// clears them here instead, before main and before any second thread
// exists: every thread starts from this one, so none holds them. The
// capabilities the mount needs stay in the permitted and effective sets,
// which an agent that is not root loses when it is started.

/*
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>

#ifndef PR_CAP_AMBIENT
#define PR_CAP_AMBIENT 47
#endif
#ifndef PR_CAP_AMBIENT_CLEAR_ALL
#define PR_CAP_AMBIENT_CLEAR_ALL 4
#endif

__attribute__((constructor)) static void agentbus_userns_clear_ambient(void) {
	const char *v = getenv("AGENTBUS_USERNS");
	if (v != NULL && strcmp(v, "child") == 0) {
		prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0);
	}
}
*/
import "C"
