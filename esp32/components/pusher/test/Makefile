CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -std=c11
BUILD := build
COMMON := ../src/ack_progress.c ../../spool/src/spool.c ../../spool/src/spool_ring.c ../../gnf1/src/gnf1.c ../../ota/src/update_json.c
INCLUDES := -Istubs -I../include -I../../spool/include -I../../gnf1/include -I../../ota/include
TESTS := ack_stall_test tls_want_yield_test endpoint_fallback_test tunnel_path_test
.PHONY: all clean
all: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do ./$$t; done
$(BUILD)/%: %.c $(COMMON) ../src/pusher.c ../include/ack_progress.h ../../../../common/endpoint_fallback.h $(wildcard stubs/*.h stubs/freertos/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) $(INCLUDES) -o $@ $< $(COMMON)
clean:
	rm -rf $(BUILD)
