SHELL := /bin/sh

LINUX_URL := https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git
LINUX_BRANCH := linux-6.18.y
LINUX_DIR := kernel/linux

.PHONY: linux-mount linux-fetch linux-status kernel-config-probe qemu-initramfs aok-initramfs qemu-boot linux-clean-prep aok-object-build aok-object-test aok-object-test-disabled aok-patch-check aok-candidate-check aok-task-test aok-task-test-disabled aok-resource-test aok-resource-test-disabled aok-eventsrc-test aok-eventsrc-test-disabled aok-appregistry-test-disabled

linux-mount:
	@if [ -d "$(LINUX_DIR)/.git" ]; then \
		echo "Linux baseline is already mounted at $(LINUX_DIR)."; \
	elif command -v hdiutil >/dev/null 2>&1; then \
		if [ ! -f .aok-linux-hfs.sparseimage ]; then \
			hdiutil create -size 4g -type SPARSE -fs 'Case-sensitive Journaled HFS+' -volname AOKLinux .aok-linux-hfs.sparseimage >/dev/null; \
		fi; \
		mkdir -p "$(LINUX_DIR)" && \
		hdiutil attach .aok-linux-hfs.sparseimage -nobrowse -mountpoint "$(CURDIR)/$(LINUX_DIR)"; \
	else \
		echo "No case-sensitive Linux volume found; create or attach .aok-linux-hfs.sparseimage." >&2; \
		exit 1; \
	fi

linux-fetch:
	@if [ ! -d "$(LINUX_DIR)/.git" ] && command -v hdiutil >/dev/null 2>&1; then \
		$(MAKE) --no-print-directory linux-mount; \
	fi
	@if [ -d "$(LINUX_DIR)/.git" ]; then \
		git -C "$(LINUX_DIR)" fetch --depth=1 origin "$(LINUX_BRANCH)" && \
		git -C "$(LINUX_DIR)" checkout --detach FETCH_HEAD; \
	else \
		mkdir -p kernel && \
		git clone --depth=1 --branch "$(LINUX_BRANCH)" "$(LINUX_URL)" "$(LINUX_DIR)"; \
	fi

linux-status:
	@if [ ! -d "$(LINUX_DIR)/.git" ]; then \
		echo "Linux baseline is missing; run 'make linux-fetch'." >&2; \
		exit 1; \
	fi
	@git -C "$(LINUX_DIR)" status --short --branch
	@git -C "$(LINUX_DIR)" log -1 --format='commit %H%nsubject %s'
	@printf '%s\n' 'AOK layers: kernel/patches/series kernel/configs kernel/kselftest'

kernel-config-probe:
	@kernel/configs/probe-stock.sh

qemu-initramfs:
	@kernel/initramfs/build-qemu.sh

aok-initramfs:
	@kernel/initramfs/build-aok.sh

qemu-boot:
	@kernel/configs/boot-qemu-arm64.sh

aok-object-build:
	@sh kernel/kselftest/build-object.sh

aok-object-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial.log" \
	 AOK_TEST_MARKER=AOK_OBJECT_TEST sh kernel/kselftest/run-object.sh

aok-object-test-disabled:
	@AOK_OBJECT_IMAGE="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/Image.disabled" \
	 AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial.disabled.log" \
	 AOK_TEST_MARKER=AOK_OBJECT_TEST \
	 AOK_TEST_ARGS=--expect-disabled sh kernel/kselftest/run-object.sh

aok-patch-check:
	@sh kernel/patches/check-series.sh

aok-task-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/task-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-task.log" \
	 AOK_TEST_MARKER=AOK_TASK_TEST sh kernel/kselftest/run-object.sh

aok-task-test-disabled:
	@AOK_OBJECT_IMAGE="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/Image.disabled" \
	 AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/task-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-task.disabled.log" \
	 AOK_TEST_MARKER=AOK_TASK_TEST \
	 AOK_TEST_ARGS=--expect-disabled sh kernel/kselftest/run-object.sh

aok-resource-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/resource-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-resource.log" \
	 AOK_TEST_MARKER=AOK_RESOURCE_TEST sh kernel/kselftest/run-object.sh

aok-resource-test-disabled:
	@AOK_OBJECT_IMAGE="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/Image.disabled" \
	 AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/resource-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-resource.disabled.log" \
	 AOK_TEST_MARKER=AOK_RESOURCE_TEST \
	 AOK_TEST_ARGS=--expect-disabled sh kernel/kselftest/run-object.sh

aok-eventsrc-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/eventsrc-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-eventsrc.log" \
	 AOK_TEST_MARKER=AOK_EVENTSRC_TEST sh kernel/kselftest/run-object.sh

aok-eventsrc-test-disabled:
	@AOK_OBJECT_IMAGE="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/Image.disabled" \
	 AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/eventsrc-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-eventsrc.disabled.log" \
	 AOK_TEST_MARKER=AOK_EVENTSRC_TEST \
	 AOK_TEST_ARGS=--expect-disabled sh kernel/kselftest/run-object.sh

.PHONY: aok-eventwake-test
aok-eventwake-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/eventwake-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-eventwake.log" \
	 AOK_TEST_MARKER=AOK_EVENTWAKE_TEST sh kernel/kselftest/run-object.sh

.PHONY: aok-appregistry-test aok-appregistry-test-disabled
aok-appregistry-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/appregistry-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-appregistry.log" \
	 AOK_TEST_MARKER=AOK_APPREG_TEST sh kernel/kselftest/run-object.sh

aok-appregistry-test-disabled:
	@AOK_OBJECT_IMAGE="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/Image.disabled" \
	 AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/appregistry-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-appregistry.disabled.log" \
	 AOK_TEST_MARKER=AOK_APPREG_TEST \
	 AOK_TEST_ARGS=--expect-disabled sh kernel/kselftest/run-object.sh

aok-candidate-check:
	@test -n "$(PATCH)" || { echo 'usage: make aok-candidate-check PATCH=kernel/patches/0003-...patch' >&2; exit 2; }
	@sh kernel/patches/check-candidate.sh "$(PATCH)"

linux-clean-prep:
	@find host/.build -depth -type f -delete 2>/dev/null || true
	@find host/.build -depth -type d -empty -delete 2>/dev/null || true
	@printf '%s\n' 'Removed generated host build output; Linux source and AOK design artifacts were preserved.'

.PHONY: runtime-test runtime-smoke aok-event-probe-test
runtime-test:
	@cd runtime && go test -race ./... && go vet ./...

aok-event-probe-test:
	@sh runtime/scripts/event-probe.sh

.PHONY: aok-initfs-boot-test
aok-initfs-boot-test:
	@sh runtime/scripts/initfs-boot.sh

runtime-smoke:
	@python3 runtime/scripts/smoke.py

.PHONY: runtime-core-smoke runtime-hitrate-smoke aok-core-test
runtime-core-smoke:
	@python3 runtime/scripts/core-smoke.py

runtime-hitrate-smoke:
	@mkdir -p kernel/.build/smoke-logs && AOK_SMOKE_LOG="$(CURDIR)/kernel/.build/smoke-logs/runtime-hitrate.log" python3 runtime/scripts/hitrate-smoke.py

.PHONY: runtime-affinity-smoke
runtime-affinity-smoke:
	@mkdir -p kernel/.build/smoke-logs && AOK_SMOKE_LOG="$(CURDIR)/kernel/.build/smoke-logs/runtime-affinity.log" python3 runtime/scripts/affinity-smoke.py

.PHONY: runtime-mvp-smoke
runtime-mvp-smoke:
	@mkdir -p kernel/.build/smoke-logs && AOK_SMOKE_LOG="$(CURDIR)/kernel/.build/smoke-logs/runtime-mvp.log" python3 runtime/scripts/mvp-smoke.py

aok-core-test:
	@AOK_INITRD="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/core-initramfs.cpio.gz" \
	 AOK_SERIAL_LOG="$${AOK_OBJECT_OUTPUT:-$(CURDIR)/kernel/.build/qemu-arm64-object}/serial-core.log" \
	 AOK_TEST_MARKER=AOK_CORE_TEST sh kernel/kselftest/run-object.sh
