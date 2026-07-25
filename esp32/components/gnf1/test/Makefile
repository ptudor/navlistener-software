# Host unit tests for the gnf1 component.
#
# gnf1 is pure buffer code (no ESP-IDF, no FreeRTOS, no I/O), which is exactly why
# Run the GNF1 buffer tests without an ESP-IDF installation.
# without a board or an ESP-IDF install:
#
#   make -C esp32/components/gnf1/test        # build + run every host test
#
# The same sources are compiled and run automatically by go/internal/ingest's
# TestESP32Gnf1FrameTypeMatchesGolden, so `make -C go check` covers them too.

CC      ?= cc
CFLAGS  ?= -O1 -Wall -Wextra -std=c11
BUILD   := build
GOLDEN  := ../../../../testdata/gnf1_frame_type.tsv

TESTS   := frame_type_matrix_test

.PHONY: all run clean
all: run

run: $(addprefix $(BUILD)/,$(TESTS))
	@set -e; for t in $(addprefix $(BUILD)/,$(TESTS)); do echo "==> $$t"; ./$$t $(GOLDEN); done

$(BUILD)/%: %.c ../src/gnf1.c ../include/gnf1.h
	@mkdir -p $(BUILD)
	$(CC) $(CFLAGS) -I../include -o $@ $< ../src/gnf1.c

clean:
	rm -rf $(BUILD)
