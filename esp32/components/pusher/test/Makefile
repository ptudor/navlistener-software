CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -std=c11
BUILD := build
SOURCES := ack_stall_test.c ../src/ack_progress.c ../../spool/src/spool.c ../../gnf1/src/gnf1.c
INCLUDES := -Istubs -I../include -I../../spool/include -I../../gnf1/include
.PHONY: all clean
all: $(BUILD)/ack_stall_test
	./$(BUILD)/ack_stall_test
$(BUILD)/ack_stall_test: $(SOURCES) ../src/pusher.c ../include/ack_progress.h $(wildcard stubs/*.h stubs/freertos/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) $(INCLUDES) -o $@ $(SOURCES)
clean:
	rm -rf $(BUILD)
