CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/report_test build/panel_settings_test
	./build/report_test ../../../../testdata/observer_details_v1.hex ../../../../testdata/observer_timing_v1.hex
	./build/panel_settings_test
build/report_test: report_test.c ../src/observer_report.c ../src/panel_control.c ../include/observer_report.h ../include/panel_control.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../gnf1/include report_test.c ../src/observer_report.c ../src/panel_control.c -o $@
build/panel_settings_test: panel_settings_test.c ../src/panel_settings.c ../src/panel_control.c ../include/panel_settings.h ../include/panel_control.h $(wildcard mocks/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Imocks -I../include panel_settings_test.c ../src/panel_settings.c ../src/panel_control.c -o $@
clean:
	rm -rf build
