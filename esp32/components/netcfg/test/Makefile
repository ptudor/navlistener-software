# Host unit tests for the netcfg component.
#
# Only the pure part is host-testable: netcfg_check.c (the station-mode config rule) has no
# ESP-IDF dependency, while netcfg.c proper needs NVS/WiFi/httpd. That split is the point of
# regression fix — the rule lives in one testable place instead of being duplicated inline in the
# boot path and the portal.
#
#   make -C esp32/components/netcfg/test        # build + run
#
# Also run by `make -C esp32 host-test`, which `make -C go check` invokes.

CC      ?= cc
CFLAGS  ?= -O1 -Wall -Wextra -std=c11
BUILD   := build

TESTS   := netcfg_validate_test

.PHONY: all run clean
all: run

run: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do echo "==> $$t"; ./$$t; done

$(BUILD)/%: %.c ../src/netcfg_check.c ../include/netcfg_check.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ $< ../src/netcfg_check.c

clean:
	rm -rf $(BUILD)
