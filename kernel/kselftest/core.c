// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/reboot.h>
#include <sys/syscall.h>
#include <unistd.h>
#include "kselftest.h"

#define CHECK(x, name) ksft_test_result(!!(x), "%s\n", name)
#define DENIED(x, error, name) do { errno = 0; long r = (x); \
 CHECK(r == -1 && errno == (error), name); } while (0)

int main(void)
{
	struct aok_core_cap policy = { .rights = AOK_CAP_ALL,
		.taint = 1, .export_mask = 1, .token_limit = 20 };
	struct aok_core_session pair, second;
	struct aok_core_io io = { .sequence = 1, .input_tokens = 3,
		.output_tokens = 4, .length = 5, .data = "hello" }, result;
	int root, cap, child, grandchild, sibling, reduced;
	struct pollfd pollfd;

	ksft_print_header();
	ksft_set_plan(46);
	root = syscall(__NR_aok_root_cap_claim);
	CHECK(root >= 0, "PID1 claims root authority");
	cap = ioctl(root, AOK_CORE_CAP_CREATE, &policy);
	CHECK(cap >= 0, "root mints inference capability");
	CHECK(fcntl(cap, F_GETFD) & FD_CLOEXEC, "capability close-on-exec");
	child = ioctl(cap, AOK_CORE_CAP_DERIVE, &policy);
	CHECK(child >= 0, "derive capability");
	grandchild = ioctl(child, AOK_CORE_CAP_DERIVE, &policy);
	CHECK(grandchild >= 0, "derive second level");
	policy.taint = 0;
	DENIED(ioctl(cap, AOK_CORE_CAP_DERIVE, &policy), EACCES, "cannot remove taint");
	policy.taint = 1;
	policy.token_limit = 21;
	DENIED(ioctl(cap, AOK_CORE_CAP_DERIVE, &policy), EACCES, "cannot raise budget");
	policy.token_limit = 20;
	policy.export_mask = 3;
	DENIED(ioctl(cap, AOK_CORE_CAP_DERIVE, &policy), EACCES, "cannot widen export mask");
	policy.export_mask = 1;
	policy.rights = AOK_CAP_INFER;
	reduced = ioctl(cap, AOK_CORE_CAP_DERIVE, &policy);
	CHECK(reduced >= 0, "attenuate rights");
	DENIED(ioctl(reduced, AOK_CORE_SESSION_CREATE, &pair), EACCES, "client cannot mint backend");
	DENIED(ioctl(reduced, AOK_CORE_CAP_DERIVE, &policy), EACCES, "cannot delegate without right");
	CHECK(ioctl(child, AOK_CORE_SESSION_CREATE, &pair) == 0, "create client/backend pair");
	DENIED(ioctl(child, AOK_CORE_SESSION_CREATE, (void *)1), EFAULT, "failed session copyout rolls back descriptors");
	CHECK((fcntl(pair.client_fd, F_GETFD) & FD_CLOEXEC) &&
	      (fcntl(pair.backend_fd, F_GETFD) & FD_CLOEXEC), "sessions close-on-exec");
	DENIED(ioctl(pair.client_fd, AOK_CORE_COMPLETE, &io), EACCES, "client cannot forge completion");
	DENIED(ioctl(pair.backend_fd, AOK_CORE_SUBMIT, &io), EACCES, "backend cannot submit");
	CHECK(ioctl(pair.client_fd, AOK_CORE_SUBMIT, &io) == 0, "submit reserves quota");
	pollfd = (struct pollfd){ .fd = pair.backend_fd, .events = POLLIN };
	CHECK(poll(&pollfd, 1, 0) == 1 && (pollfd.revents & POLLIN), "backend readiness event");
	DENIED(ioctl(pair.backend_fd, AOK_CORE_TAKE, (void *)1), EFAULT, "faulting take leaves request pending");
	DENIED(ioctl(pair.client_fd, AOK_CORE_SUBMIT, &io), EBUSY, "single outstanding submission");
	CHECK(ioctl(pair.backend_fd, AOK_CORE_TAKE, &result) == 0 &&
	      result.taint == 1 && result.length == 5 && !memcmp(result.data, "hello", 5),
	      "backend receives payload with kernel taint");
	DENIED(ioctl(pair.backend_fd, AOK_CORE_TAKE, &result), EAGAIN, "backend take at most once");
	io.output_tokens = 5;
	DENIED(ioctl(pair.backend_fd, AOK_CORE_COMPLETE, &io), EDQUOT, "completion cannot exceed reservation");
	io.output_tokens = 2;
	io.length = 2;
	memcpy(io.data, "ok", 2);
	CHECK(ioctl(pair.backend_fd, AOK_CORE_COMPLETE, &io) == 0, "trusted backend completes actual payload");
	pollfd = (struct pollfd){ .fd = pair.client_fd, .events = POLLIN };
	CHECK(poll(&pollfd, 1, 0) == 1 && (pollfd.revents & POLLIN), "client completion readiness");
	DENIED(ioctl(pair.backend_fd, AOK_CORE_COMPLETE, &io), ESTALE, "duplicate completion rejected");
	CHECK(ioctl(pair.client_fd, AOK_CORE_RESULT, &result) == 0 &&
	      result.tokens_used == 5 && result.taint == 1 && result.state == AOK_INFER_DONE,
	      "result carries committed usage and taint");
	CHECK(ioctl(pair.client_fd, AOK_CORE_EXPORT, &result) == 0, "matching export policy permits result");
	io.sequence = 2;
	io.taint = 2;
	CHECK(ioctl(pair.client_fd, AOK_CORE_SUBMIT, &io) == 0 &&
	      ioctl(pair.backend_fd, AOK_CORE_TAKE, &result) == 0 &&
	      ioctl(pair.backend_fd, AOK_CORE_COMPLETE, &io) == 0,
	      "new turn monotonically adds taint");
	DENIED(ioctl(pair.client_fd, AOK_CORE_EXPORT, &result), EACCES, "tainted external export denied");
	policy.rights = AOK_CAP_ALL;
	sibling = ioctl(cap, AOK_CORE_CAP_DERIVE, &policy);
	CHECK(sibling >= 0 && ioctl(sibling, AOK_CORE_SESSION_CREATE, &second) == 0,
	      "sibling uses shared ancestor budget");
	io.sequence = 1;
	io.input_tokens = 11;
	io.output_tokens = 0;
	DENIED(ioctl(second.client_fd, AOK_CORE_SUBMIT, &io), EDQUOT, "shared ancestor quota enforced");
	io.input_tokens = 10;
	CHECK(ioctl(second.client_fd, AOK_CORE_SUBMIT, &io) == 0, "reserve remaining ancestor budget");
	io.sequence = 3;
	io.input_tokens = 1;
	DENIED(ioctl(pair.client_fd, AOK_CORE_SUBMIT, &io), EDQUOT, "concurrent reservations cannot oversubscribe");
	CHECK(ioctl(second.client_fd, AOK_CORE_CANCEL, 0) == 0, "cancel releases reservation");
	CHECK(ioctl(pair.client_fd, AOK_CORE_SUBMIT, &io) == 0, "released quota reusable");
	CHECK(ioctl(child, AOK_CORE_CAP_REVOKE, 0) == 0, "revoke child");
	DENIED(ioctl(grandchild, AOK_CORE_CAP_CHECK, 0), EACCES, "ancestor revoke reaches descendants");
	DENIED(ioctl(pair.backend_fd, AOK_CORE_TAKE, &result), EACCES, "revocation denies live inference");
	CHECK(ioctl(sibling, AOK_CORE_CAP_CHECK, 0) == 0, "sibling remains live");
	pollfd = (struct pollfd){ .fd = pair.client_fd, .events = POLLIN };
	CHECK(poll(&pollfd, 1, 0) == 1 && (pollfd.revents & POLLERR), "revocation readiness error");
	close(pair.client_fd);
	close(pair.backend_fd);
	io.sequence = 2;
	io.input_tokens = 10;
	CHECK(ioctl(second.client_fd, AOK_CORE_SUBMIT, &io) == 0 &&
	      ioctl(second.backend_fd, AOK_CORE_TAKE, &result) == 0,
	      "closed revoked request refunds pending reservation");
	CHECK(ioctl(second.client_fd, AOK_CORE_CANCEL, 0) == 0, "cancel running request");
	io.sequence = 3;
	io.input_tokens = 1;
	DENIED(ioctl(second.client_fd, AOK_CORE_SUBMIT, &io), EDQUOT, "running cancellation charges reserved ceiling");
	DENIED(ioctl(second.backend_fd, AOK_CORE_COMPLETE, &io), ESTALE, "late completion after cancellation rejected");
	CHECK(ioctl(cap, AOK_CORE_CAP_REVOKE, 0) == 0 &&
	      ioctl(sibling, AOK_CORE_CAP_CHECK, 0) == -1 && errno == EACCES,
	      "root capability revocation reaches surviving sibling");
	close(second.client_fd);
	close(second.backend_fd);
	close(root); close(cap); close(child); close(grandchild); close(sibling); close(reduced);
	printf("AOK_CORE_TEST=%s\n", ksft_get_fail_cnt() ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;) pause();
	}
	return ksft_get_fail_cnt() ? 1 : 0;
}
