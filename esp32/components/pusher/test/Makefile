CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -std=c11
BUILD := build
COMMON := ../src/ack_progress.c ../../spool/src/spool.c ../../gnf1/src/gnf1.c
INCLUDES := -Istubs -I../include -I../../spool/include -I../../gnf1/include
TESTS := ack_stall_test tls_want_yield_test
.PHONY: all clean
all: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do ./$$t; done
$(BUILD)/%: %.c $(COMMON) ../src/pusher.c ../include/ack_progress.h $(wildcard stubs/*.h stubs/freertos/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) $(INCLUDES) -o $@ $< $(COMMON)
clean:
	rm -rf $(BUILD)
