# Host unit tests for the netcfg component.
#
# The shared validation rule runs directly. The actual storage implementation
# also runs against an NVS atomic-blob backend with write/commit/power-cut faults.
# Portal WiFi/httpd and physical flash behavior still require on-target testing.
#
#   make -C esp32/components/netcfg/test        # build + run
#
# Also run by `make -C esp32 host-test`, which `make -C go check` invokes.

CC      ?= cc
CFLAGS  ?= -O1 -Wall -Wextra -std=c11
BUILD   := build

TESTS   := netcfg_validate_test netcfg_storage_test netcfg_form_test

.PHONY: all run clean
all: run

run: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do echo "==> $$t"; ./$$t; done

$(BUILD)/netcfg_storage_test: netcfg_storage_test.c ../src/netcfg_storage.c ../src/netcfg_check.c ../include/netcfg.h $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -Imocks -I../include -o $@ netcfg_storage_test.c ../src/netcfg_storage.c ../src/netcfg_check.c

$(BUILD)/netcfg_form_test: netcfg_form_test.c ../src/netcfg_form.c ../include/netcfg_form.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ netcfg_form_test.c ../src/netcfg_form.c

$(BUILD)/%: %.c ../src/netcfg_check.c ../include/netcfg_check.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ $< ../src/netcfg_check.c

clean:
	rm -rf $(BUILD)
