# navfeeder-esp — host-side test entry point.
#
# The FIRMWARE build is ESP-IDF/CMake and lives behind ./build-navfeeder-esp.sh; this
# Makefile is deliberately NOT a second way to build it. It runs the component unit tests
# that need no board, no flash, and no ESP-IDF installation.
# validation rule).
#
#   make -C esp32 host-test        # every component's host tests
#   make -C esp32 clean            # remove their build output
#
# `make -C go check` runs host-test, so an ESP32 regression in this shared logic fails the
# ordinary Go gate on a machine with no ESP-IDF at all. Firmware-only code (UART, TLS, LCD,
# FreeRTOS tasks) is still covered only by the real idf.py build.

# Every components/<name>/test/ directory with a Makefile.
TEST_DIRS := $(dir $(wildcard components/*/test/Makefile))

.PHONY: host-test clean help
host-test:
	@set -e; for d in $(TEST_DIRS); do $(MAKE) --no-print-directory -C $$d; done

clean:
	@set -e; for d in $(TEST_DIRS); do $(MAKE) --no-print-directory -C $$d clean; done

help:
	@echo 'navfeeder-esp host tests:'
	@echo '  make host-test   build + run every components/*/test suite'
	@echo '  make clean       remove host-test build output'
	@echo ''
	@echo 'Firmware build (needs ESP-IDF):  ./build-navfeeder-esp.sh'
