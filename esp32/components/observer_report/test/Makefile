CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/report_test
	./build/report_test ../../../../testdata/observer_details_v1.hex ../../../../testdata/observer_timing_v1.hex
build/report_test: report_test.c ../src/observer_report.c ../src/panel_control.c ../include/observer_report.h ../include/panel_control.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../gnf1/include report_test.c ../src/observer_report.c ../src/panel_control.c -o $@
clean:
	rm -rf build
