// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <sched.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include "kselftest.h"

#ifndef __aarch64__
#error "This experimental syscall allocation is tested on arm64 only"
#endif

#define NR_AOK_CREATE __NR_aok_aproc_create
#define NR_AOK_INSPECT __NR_aok_object_inspect
#define NR_AOK_DUPLICATE __NR_aok_handle_duplicate
#define NR_AOK_ROOT_CLAIM __NR_aok_root_cap_claim
#define NR_AOK_SPAWN __NR_aok_aproc_spawn
#define NR_AOK_FREEZE __NR_aok_aproc_freeze
#define NR_AOK_RESUME __NR_aok_aproc_resume
#define NR_AOK_ABORT __NR_aok_aproc_abort
#define NR_AOK_STATUS __NR_aok_aproc_status
#define NR_AOK_BUDGET_SET __NR_aok_budget_set
#define NR_AOK_BUDGET_GET __NR_aok_budget_get
#define NR_AOK_TOKEN_USAGE __NR_aok_token_usage

_Static_assert(sizeof(struct aok_budget) == 64, "budget layout");
_Static_assert(sizeof(struct aok_budget_status) == 96, "budget status layout");
_Static_assert(sizeof(struct aok_token_usage) == 64, "token usage layout");

#define AOK_RESOURCE_TEST_PLAN 53

static int root_fd = -1;
static struct aok_aproc_create_args create_args = { .size = 40, };

#define EXPECT_ERR(expr, error, name) do { \
	errno = 0; \
	long result = (expr); \
	int saved_errno = errno; \
	ksft_test_result(result == -1 && saved_errno == (error), \
			 "%s (ret=%ld errno=%d)\n", name, result, saved_errno); \
	if (result >= 0 && result != 0) close(result); \
} while (0)

static void drain_zombies(void)
{
	while (waitpid(-1, NULL, WNOHANG) > 0)
		;
}

static int create_living(struct aok_object_info *info,
			 struct aok_aproc_create_result *result)
{
	long r;

	create_args.parent_job_fd = root_fd;
	info->size = sizeof(*info);
	result->size = sizeof(*result);
	r = syscall(NR_AOK_CREATE, &create_args, info, result);
	if (r == 0) {
		for (;;)
			pause();
	}
	return r;
}

static int budget_set(int fd, struct aok_budget *budget)
{
	budget->size = sizeof(*budget);
	return syscall(NR_AOK_BUDGET_SET, fd, budget);
}

static int budget_get(int fd, struct aok_budget_status *status)
{
	status->size = sizeof(*status);
	return syscall(NR_AOK_BUDGET_GET, fd, status);
}

static int token_usage(int fd, struct aok_token_usage *usage)
{
	usage->size = sizeof(*usage);
	return syscall(NR_AOK_TOKEN_USAGE, fd, usage);
}

static void finish(int fail)
{
	printf("AOK_RESOURCE_TEST=%s\n", fail ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(fail);
}

int main(int argc, char **argv)
{
	struct aok_aproc_create_result result = {};
	struct aok_object_info first = {};
	struct aok_budget budget = {};
	struct aok_budget_status st = {};
	struct aok_token_usage usage = {};
	struct aok_aproc_event ev;
	long page = sysconf(_SC_PAGESIZE);
	char *guard;
	int fd, fd2, ro_fd, zero_fd;
	bool ok;

	if (getpid() == 1) {
		mkdir("/proc", 0755);
		mkdir("/dev", 0755);
		mount("proc", "/proc", "proc", 0, NULL);
		mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	ksft_print_header();
	if (argc == 2 && !strcmp(argv[1], "--expect-disabled")) {
		ksft_set_plan(8);
		EXPECT_ERR(syscall(NR_AOK_ROOT_CLAIM), ENOSYS, "disabled claim");
		EXPECT_ERR(syscall(NR_AOK_CREATE, &create_args, &first, &result), ENOSYS, "disabled create");
		EXPECT_ERR(syscall(NR_AOK_INSPECT, -1, &first), ENOSYS, "disabled inspect");
		EXPECT_ERR(syscall(NR_AOK_SPAWN, -1, &(struct aok_task_spawn_attr){ .size = 40, }), ENOSYS, "disabled spawn");
		EXPECT_ERR(syscall(NR_AOK_BUDGET_SET, -1, &budget), ENOSYS, "disabled budget set");
		EXPECT_ERR(syscall(NR_AOK_BUDGET_GET, -1, &st), ENOSYS, "disabled budget get");
		EXPECT_ERR(syscall(NR_AOK_TOKEN_USAGE, -1, &usage), ENOSYS, "disabled token usage");
		EXPECT_ERR(syscall(NR_AOK_DUPLICATE, -1, 0), ENOSYS, "disabled duplicate");
		ksft_print_cnts();
		finish(ksft_get_fail_cnt() != 0);
	}
	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in the QEMU test initramfs\n");
	ksft_set_plan(AOK_RESOURCE_TEST_PLAN);

	root_fd = syscall(NR_AOK_ROOT_CLAIM);
	ksft_test_result(root_fd >= 0, "PID1 claims root capability\n");

	fd = create_living(&first, &result);
	if (fd < 0 || result.pidfd < 0)
		finish(1);
	ksft_test_result(first.rights == AOK_RIGHTS_APROC_INITIAL,
			 "aproc grants budget rights initially\n");

	/* Unset limits read back as U64_MAX with an empty ledger. */
	ok = !budget_get(fd, &st) && st.cpu_usec_limit == UINT64_MAX &&
	     st.memory_bytes_limit == UINT64_MAX &&
	     st.token_reserve == UINT64_MAX && st.token_hard_limit == UINT64_MAX &&
	     !st.tokens_used && !st.tokens_cached &&
	     st.res_state == AOK_RES_STATE_OK;
	ksft_test_result(ok, "initial budget is unlimited and idle\n");

	/* Narrow every dimension, then read the values back. */
	budget.cpu_usec_limit = 1000000000;
	budget.memory_bytes_limit = 4UL << 20;
	budget.token_reserve = 1000;
	budget.token_hard_limit = 1000;
	ok = !budget_set(fd, &budget);
	ksft_test_result(ok, "budget set narrows all dimensions\n");
	ok = !budget_get(fd, &st) && st.cpu_usec_limit == 1000000000 &&
	     st.memory_bytes_limit == (4UL << 20) &&
	     st.token_reserve == 1000 && st.token_hard_limit == 1000;
	ksft_test_result(ok, "budget get returns the narrowed limits\n");

	/* CPU and memory are observed from the bound tasks. */
	ok = !budget_get(fd, &st) && st.cpu_usec_used < st.cpu_usec_limit &&
	     st.memory_bytes_used < st.memory_bytes_limit;
	ksft_test_result(ok, "cpu and memory usage are observed\n");

	/* Widening any single dimension is rejected atomically. */
	budget.cpu_usec_limit = 2000000000;
	EXPECT_ERR(budget_set(fd, &budget), EPERM, "widen cpu rejected");
	ok = !budget_get(fd, &st) && st.cpu_usec_limit == 1000000000;
	ksft_test_result(ok, "rejected set changes nothing\n");

	/* Partial narrowing keeps the other dimensions unchanged. */
	budget.cpu_usec_limit = 500000000;
	ok = !budget_set(fd, &budget) && !budget_get(fd, &st) &&
	     st.cpu_usec_limit == 500000000 && st.token_hard_limit == 1000;
	ksft_test_result(ok, "partial narrowing is atomic\n");

	budget.token_reserve = 1001;
	EXPECT_ERR(budget_set(fd, &budget), EINVAL, "reserve above hard limit");
	budget.token_reserve = 1000;

	/* Structural validation. */
	budget.flags = 1;
	EXPECT_ERR(budget_set(fd, &budget), EINVAL, "unknown budget flags");
	budget.flags = 0;
	budget.reserved[1] = 1;
	EXPECT_ERR(budget_set(fd, &budget), EINVAL, "budget reserved field");
	budget.reserved[1] = 0;
	budget.size = sizeof(budget) - 1;
	EXPECT_ERR(syscall(NR_AOK_BUDGET_SET, fd, &budget), EINVAL,
		   "short budget");
	budget.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_BUDGET_SET, fd, &budget), EINVAL,
		   "oversized budget");
	budget.size = sizeof(budget);
	EXPECT_ERR(syscall(NR_AOK_BUDGET_SET, fd, NULL), EFAULT, "null budget");
	{
		struct {
			struct aok_budget budget;
			uint64_t tail;
		} extended;

		memset(&extended, 0, sizeof(extended));
		memcpy(&extended.budget, &budget, sizeof(budget));
		extended.budget.size = sizeof(extended);
		extended.tail = 1;
		EXPECT_ERR(syscall(NR_AOK_BUDGET_SET, fd, &extended.budget),
			   E2BIG, "nonzero budget tail rejected");
		memset(&extended, 0, sizeof(extended));
		memcpy(&extended.budget, &budget, sizeof(budget));
		extended.budget.size = sizeof(extended);
		ok = !syscall(NR_AOK_BUDGET_SET, fd, &extended.budget);
		ksft_test_result(ok, "zero extended budget accepted\n");
	}
	EXPECT_ERR(syscall(NR_AOK_BUDGET_GET, fd, NULL), EFAULT,
		   "null budget status");
	st.size = sizeof(st) - 1;
	EXPECT_ERR(syscall(NR_AOK_BUDGET_GET, fd, &st), EINVAL,
		   "short budget status");
	memset(&st, 0xff, sizeof(st));
	st.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_BUDGET_GET, fd, &st), EINVAL,
		   "oversized budget status");
	{
		struct {
			struct aok_budget_status status;
			uint64_t tail;
		} extended;

		memset(&extended, 0xff, sizeof(extended));
		extended.status.size = sizeof(extended);
		ok = !syscall(NR_AOK_BUDGET_GET, fd, &extended.status) &&
		     extended.tail == 0;
		ksft_test_result(ok, "extended budget status zero filled\n");
	}

	/* Usage structural validation. */
	usage.usage_id = 1;
	usage.usage_seq = 1;
	usage.input_tokens = 10;
	usage.output_tokens = 5;
	EXPECT_ERR(syscall(NR_AOK_TOKEN_USAGE, fd, NULL), EFAULT, "null usage");
	usage.size = sizeof(usage) - 1;
	EXPECT_ERR(syscall(NR_AOK_TOKEN_USAGE, fd, &usage), EINVAL, "short usage");
	usage.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_TOKEN_USAGE, fd, &usage), EINVAL, "oversized usage");
	usage.size = sizeof(usage);
	usage.reserved[0] = 1;
	EXPECT_ERR(token_usage(fd, &usage), EINVAL, "usage reserved field");
	usage.reserved[0] = 0;
	usage.usage_id = 0;
	EXPECT_ERR(token_usage(fd, &usage), EINVAL, "zero usage id");
	usage.usage_id = 1;
	usage.usage_seq = 0;
	EXPECT_ERR(token_usage(fd, &usage), EINVAL, "zero usage sequence");
	usage.usage_seq = 1;

	/* Rights gate the new calls before any state check. */
	ro_fd = syscall(NR_AOK_DUPLICATE, fd, AOK_RIGHT_INSPECT);
	EXPECT_ERR(budget_set(ro_fd, &budget), EACCES, "set policy right required");
	EXPECT_ERR(token_usage(ro_fd, &usage), EACCES, "account right required");
	ok = !budget_get(ro_fd, &st);
	ksft_test_result(ok, "inspect right suffices for budget get\n");
	close(ro_fd);
	zero_fd = syscall(NR_AOK_DUPLICATE, fd, 0);
	EXPECT_ERR(budget_get(zero_fd, &st), EACCES, "get right required");
	EXPECT_ERR(read(zero_fd, &ev, sizeof(ev)), EACCES, "read right required");
	close(zero_fd);

	/* Token ledger: settlement, idempotency, ordering, cache split. */
	ok = !token_usage(fd, &usage) && !budget_get(fd, &st) &&
	     st.tokens_used == 15;
	ksft_test_result(ok, "usage settles input plus output\n");
	ok = !token_usage(fd, &usage) && !budget_get(fd, &st) &&
	     st.tokens_used == 15;
	ksft_test_result(ok, "replayed usage does not double bill\n");
	usage.usage_id = 2;
	EXPECT_ERR(token_usage(fd, &usage), EINVAL, "stale usage sequence");
	usage.usage_id = 3;
	usage.usage_seq = 9;
	usage.input_tokens = 20;
	usage.output_tokens = 0;
	ok = !token_usage(fd, &usage) && !budget_get(fd, &st) &&
	     st.tokens_used == 35;
	ksft_test_result(ok, "sequence gaps are accepted\n");
	usage.usage_id = 4;
	usage.usage_seq = 10;
	usage.input_tokens = 0;
	usage.output_tokens = 0;
	usage.cached_tokens = 7;
	ok = !token_usage(fd, &usage) && !budget_get(fd, &st) &&
	     st.tokens_used == 35 && st.tokens_cached == 7;
	ksft_test_result(ok, "cached tokens accounted separately\n");
	usage.cached_tokens = 0;

	/* Narrowing into the pressure band reports and records it. */
	budget.cpu_usec_limit = 500000000;
	budget.memory_bytes_limit = 4UL << 20;
	budget.token_reserve = 40;
	budget.token_hard_limit = 40;
	ok = !budget_set(fd, &budget) && !budget_get(fd, &st) &&
	     st.res_state == AOK_RES_STATE_PRESSURE;
	ksft_test_result(ok, "narrowing into pressure band reports state\n");

	/* Reaching the hard limit exactly is allowed and becomes OVER. */
	usage.usage_id = 5;
	usage.usage_seq = 11;
	usage.input_tokens = 5;
	ok = !token_usage(fd, &usage) && !budget_get(fd, &st) &&
	     st.tokens_used == 40 && st.res_state == AOK_RES_STATE_OVER;
	ksft_test_result(ok, "reaching the hard limit becomes over\n");

	/* Beyond the hard limit is refused after conservative settlement and freeze. */
	usage.usage_id = 6;
	usage.usage_seq = 12;
	usage.input_tokens = 1;
	EXPECT_ERR(token_usage(fd, &usage), EDQUOT, "usage beyond hard limit");
	ok = !budget_get(fd, &st) && st.tokens_used == 41 &&
	     st.res_state == AOK_RES_STATE_OVER;
	ksft_test_result(ok, "over-limit usage is conservatively settled\n");

	/* Budget events land in the ordered ring with state transitions. */
	ok = read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
	     ev.kind == AOK_EVENT_TASK_JOINED && ev.event_seq == 1;
	ok = ok && read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
		ev.kind == AOK_EVENT_BUDGET &&
		ev.state == AOK_RES_STATE_PRESSURE && ev.event_seq == 2;
	ok = ok && read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
		ev.kind == AOK_EVENT_BUDGET &&
		ev.state == AOK_RES_STATE_OVER && ev.event_seq == 3;
	ok = ok && read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
		ev.kind == AOK_EVENT_STATE && ev.state == AOK_APROC_STATE_FROZEN &&
		ev.event_seq == 4;
	ok = ok && read(fd, &ev, sizeof(ev)) == 0;
	ksft_test_result(ok, "budget events are ordered and drain to zero\n");

	/* The hard token transition also freezes the aproc task set. */
	ok = !syscall(NR_AOK_RESUME, fd);
	ksft_test_result(ok, "over-limit domain resumes explicitly\n");

	/* Saturation must never wrap, even at U64_MAX traffic. */
	fd2 = create_living(&first, &result);
	if (fd2 < 0)
		finish(1);
	usage.usage_id = 1;
	usage.usage_seq = 1;
	usage.input_tokens = UINT64_MAX;
	usage.output_tokens = UINT64_MAX;
	ok = !token_usage(fd2, &usage) && !budget_get(fd2, &st) &&
	     st.tokens_used == UINT64_MAX;
	ksft_test_result(ok, "token sum saturates\n");
	usage.usage_id = 2;
	usage.usage_seq = 2;
	usage.input_tokens = 1;
	usage.output_tokens = 0;
	usage.cached_tokens = UINT64_MAX;
	ok = !token_usage(fd2, &usage) && !budget_get(fd2, &st) &&
	     st.tokens_used == UINT64_MAX && st.tokens_cached == UINT64_MAX;
	ksft_test_result(ok, "ledger saturates without wrapping\n");
	usage.cached_tokens = 0;

	/* Budget calls stay compatible with the freeze lifecycle. */
	ok = !syscall(NR_AOK_FREEZE, fd2);
	budget.cpu_usec_limit = UINT64_MAX;
	budget.memory_bytes_limit = UINT64_MAX;
	budget.token_reserve = UINT64_MAX;
	budget.token_hard_limit = UINT64_MAX;
	ok = ok && !budget_set(fd2, &budget);
	usage.usage_id = 3;
	usage.usage_seq = 3;
	ok = ok && !token_usage(fd2, &usage);
	ok = ok && syscall(NR_AOK_RESUME, fd2) == 0;
	ksft_test_result(ok, "budget calls work across freeze and resume\n");

	/* Terminal aproc: set and usage refuse, observation survives. */
	ok = !syscall(NR_AOK_ABORT, fd2);
	budget.cpu_usec_limit = 123;
	EXPECT_ERR(budget_set(fd2, &budget), EINVAL, "set on failed aproc");
	usage.usage_id = 4;
	usage.usage_seq = 4;
	EXPECT_ERR(token_usage(fd2, &usage), EINVAL, "usage on failed aproc");
	ok = ok && !budget_get(fd2, &st) && st.tokens_used == UINT64_MAX;
	ksft_test_result(ok, "terminal aproc keeps observable ledger\n");
	close(result.pidfd);
	close(fd2);

	/* Guard-page fault on the usage record, then finish the primary. */
	guard = mmap(NULL, page * 2, PROT_READ | PROT_WRITE,
		     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (guard == MAP_FAILED || mprotect(guard + page, page, PROT_NONE))
		finish(1);
	*(uint64_t *)(guard + page - sizeof(uint64_t)) = sizeof(usage);
	EXPECT_ERR(syscall(NR_AOK_TOKEN_USAGE, fd,
			   guard + page - sizeof(uint64_t)), EFAULT,
		   "partial usage fault");
	munmap(guard, page * 2);

	syscall(NR_AOK_ABORT, fd);
	ok = !budget_get(fd, &st) && st.tokens_used == 41;
	ksft_test_result(ok, "primary ledger survives abort\n");

	/* An observed CPU limit fails closed through the same freeze path. */
	fd2 = create_living(&first, &result);
	if (fd2 < 0)
		finish(1);
	memset(&budget, 0, sizeof(budget));
	budget.cpu_usec_limit = 0;
	ok = !budget_set(fd2, &budget) && !budget_get(fd2, &st) &&
	     st.res_state == AOK_RES_STATE_OVER;
	ksft_test_result(ok, "observed CPU limit freezes domain\n");
	syscall(NR_AOK_ABORT, fd2);
	close(result.pidfd);
	close(fd2);

	/* RSS is sampled independently and converted from pages to bytes. */
	fd2 = create_living(&first, &result);
	if (fd2 < 0)
		finish(1);
	memset(&budget, 0, sizeof(budget));
	budget.cpu_usec_limit = UINT64_MAX;
	budget.memory_bytes_limit = 0;
	budget.token_reserve = UINT64_MAX;
	budget.token_hard_limit = UINT64_MAX;
	ok = !budget_set(fd2, &budget) && !budget_get(fd2, &st) &&
	     st.memory_bytes_used > 0 &&
	     st.res_state == AOK_RES_STATE_OVER;
	ksft_test_result(ok, "observed RSS limit freezes domain\n");
	syscall(NR_AOK_ABORT, fd2);
	close(result.pidfd);
	close(fd2);
	drain_zombies();
	close(root_fd);
	ksft_print_cnts();
	finish(ksft_get_fail_cnt() != 0 ||
	       ksft_test_num() != AOK_RESOURCE_TEST_PLAN);
	return 0;
}
