# navfeeder-esp — host tests and explicit hardware workflows.
#
# The ordinary S3 firmware build remains behind ./build-navfeeder-esp.sh. The
# first-boot-flash target is the attended S3 manufacturing exception: it builds,
# flashes, monitors, and captures the persistent setup label credential.
# Host-test targets need no board, flash, or ESP-IDF installation.
#
#   make -C esp32 host-test        # every component's host tests
#   make -C esp32 clean            # remove their build output
#   PORT=/dev/cu.usbmodem... make -C esp32 first-boot-flash
#
# `make -C go check` runs host-test, so an ESP32 regression in this shared logic fails the
# ordinary Go gate on a machine with no ESP-IDF at all. Firmware-only code (UART, TLS, LCD,
# FreeRTOS tasks) is still covered only by the real idf.py build.

# Every components/<name>/test/ directory with a Makefile.
TEST_DIRS := $(dir $(wildcard components/*/test/Makefile))
S3_BUILD_DIR ?= build/s3-layout3
FIRST_BOOT_DIR ?=

.PHONY: host-test first-boot-flash clean help
host-test:
	@set -e; for d in $(TEST_DIRS); do $(MAKE) --no-print-directory -C $$d; done

first-boot-flash:
	@PORT='$(PORT)' S3_BUILD_DIR='$(S3_BUILD_DIR)' \
		FIRST_BOOT_DIR='$(FIRST_BOOT_DIR)' \
		./tools/first_boot_flash.sh

clean:
	@set -e; for d in $(TEST_DIRS); do $(MAKE) --no-print-directory -C $$d clean; done

help:
	@echo 'navfeeder-esp targets:'
	@echo '  make host-test         build + run every components/*/test suite'
	@echo '  make clean             remove host-test build output'
	@echo '  PORT=/dev/... make first-boot-flash'
	@echo '                          build/flash/monitor the S3 and capture the'
	@echo '                          setup label credential in a'
	@echo '                          new private directory under build/s3-layout3'
	@echo ''
	@echo 'Firmware build (needs ESP-IDF):  ./build-navfeeder-esp.sh'
	@echo 'First-boot S3 flash also needs IDF_PATH and never erases NVS.'
