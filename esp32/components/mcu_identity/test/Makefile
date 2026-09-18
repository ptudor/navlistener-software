# Host unit tests for the portable commissioning core (no ESP-IDF, no board).
#
#   make -C esp32/components/mcu_identity/test
#
# Also run by `make -C esp32 host-test`, which `make -C go check` invokes. The shared
# fixtures are written by the Go reference implementation; see core_test.c.

CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
FIXTURES := $(wildcard ../../../../common/fixtures/commissioning-*-v1.hex)
.PHONY: all clean
all: build/core_test
	./build/core_test
	python3 ../../../tools/test_commission_report.py
build/core_test: core_test.c reference.h ../src/mcu_identity_core.c ../include/mcu_identity_core.h $(FIXTURES)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include core_test.c ../src/mcu_identity_core.c -o $@
clean:
	rm -rf build
