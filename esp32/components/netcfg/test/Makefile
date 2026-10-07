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

# netcfg_check calls into netcfg_tunnel for an enabled profile, so every binary that
# links the validation rule links the tunnel parser too.
CHECK   := ../src/netcfg_check.c ../src/netcfg_tunnel.c
CHECK_H := ../include/netcfg_check.h ../include/netcfg_tunnel.h

TESTS   := netcfg_validate_test netcfg_storage_test netcfg_setup_test \
	netcfg_reset_setup_test netcfg_form_test netcfg_prov_payload_test \
	netcfg_tunnel_test netcfg_portal_test netcfg_portal_insecure_test

# The portal decoder is built twice from one test source: as shipped (no insecure
# checkbox) and as a bench build with it, both with the compiled NVF_INSECURE default on
# to show it never reaches the record.
PORTAL   := ../src/netcfg_portal.c ../src/netcfg_form.c $(CHECK)
PORTAL_H := ../include/netcfg_portal.h ../include/netcfg_form.h $(CHECK_H)
PORTAL_FLAGS := -DCONFIG_NVF_WIREGUARD=1 -DCONFIG_NVF_INSECURE=1 -Imocks -I../include

.PHONY: all run clean
all: run

run: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do echo "==> $$t"; ./$$t; done

$(BUILD)/netcfg_portal_test: netcfg_portal_test.c $(PORTAL) $(PORTAL_H) $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) $(PORTAL_FLAGS) -o $@ netcfg_portal_test.c $(PORTAL)

$(BUILD)/netcfg_portal_insecure_test: netcfg_portal_test.c $(PORTAL) $(PORTAL_H) $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) $(PORTAL_FLAGS) -DCONFIG_NVF_ALLOW_INSECURE_PORTAL=1 -o $@ netcfg_portal_test.c $(PORTAL)

$(BUILD)/netcfg_storage_test: netcfg_storage_test.c ../src/netcfg_storage.c $(CHECK) ../include/netcfg.h $(CHECK_H) $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -Imocks -I../include -o $@ netcfg_storage_test.c ../src/netcfg_storage.c $(CHECK)

$(BUILD)/netcfg_form_test: netcfg_form_test.c ../src/netcfg_form.c ../include/netcfg_form.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ netcfg_form_test.c ../src/netcfg_form.c

$(BUILD)/netcfg_setup_test: netcfg_setup_test.c ../src/netcfg_setup.c ../src/netcfg_setup.h $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -Imocks -I../src -o $@ netcfg_setup_test.c ../src/netcfg_setup.c

$(BUILD)/netcfg_reset_setup_test: netcfg_reset_setup_test.c ../src/netcfg_storage.c ../src/netcfg_setup.c $(CHECK) ../include/netcfg.h ../src/netcfg_setup.h $(CHECK_H) $(wildcard mocks/*.h mocks/sys/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -Imocks -I../include -I../src -o $@ netcfg_reset_setup_test.c ../src/netcfg_storage.c ../src/netcfg_setup.c $(CHECK)

$(BUILD)/netcfg_prov_payload_test: netcfg_prov_payload_test.c ../src/netcfg_prov_payload.c ../src/netcfg_prov_payload.h $(CHECK) $(CHECK_H)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../src -I../include -o $@ netcfg_prov_payload_test.c ../src/netcfg_prov_payload.c $(CHECK)

$(BUILD)/%: %.c $(CHECK) $(CHECK_H)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ $< $(CHECK)

clean:
	rm -rf $(BUILD)
