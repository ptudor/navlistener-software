# Host unit tests for the pure configuration-recovery gesture state machine.

CC      ?= cc
CFLAGS  ?= -O1 -Wall -Wextra -std=c11
BUILD   := build
TESTS   := config_recovery_test

.PHONY: all run clean
all: run

run: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do echo "==> $$t"; ./$$t; done

$(BUILD)/%: %.c ../src/config_recovery.c ../include/config_recovery.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ $< ../src/config_recovery.c

clean:
	rm -rf $(BUILD)
