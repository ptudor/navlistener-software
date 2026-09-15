CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/pulse_test
	./build/pulse_test
	python3 ../../../tools/test_timing_plot.py
build/pulse_test: pulse_test.c ../src/pulse_stats.c ../include/pulse_stats.h ../../observer_report/include/timing_report.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../observer_report/include pulse_test.c ../src/pulse_stats.c -o $@
clean:
	rm -rf build
