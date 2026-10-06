CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/probe_test build/status_test build/solution_test build/reception_test
	./build/probe_test
	./build/status_test
	./build/solution_test
	./build/reception_test
build/status_test: status_test.c ../src/gnss_status.c ../include/gnss_status.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include status_test.c ../src/gnss_status.c -lm -o $@
build/probe_test: probe_test.c ../src/ubx.c ../src/ubx_probe.c ../include/ubx_probe.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../gnf1/include probe_test.c ../src/ubx.c ../src/ubx_probe.c ../../gnf1/src/gnf1.c -o $@
build/solution_test: solution_test.c ../src/ubx.c ../include/ubx.h ../../../../common/receiver_solution.h ../../../../testdata/receiver_solution_v1.txt
	@mkdir -p build
	$(CC) $(CFLAGS) -DGOLDEN='"../../../../testdata/receiver_solution_v1.txt"' -I../include -I../../gnf1/include solution_test.c ../src/ubx.c ../../gnf1/src/gnf1.c -o $@
build/reception_test: reception_test.c ../src/ubx.c ../include/ubx.h ../../../../common/reception_data.h ../../../../testdata/reception_data_v2.txt
	@mkdir -p build
	$(CC) $(CFLAGS) -DGOLDEN='"../../../../testdata/reception_data_v2.txt"' -I../include -I../../gnf1/include reception_test.c ../src/ubx.c ../../gnf1/src/gnf1.c -o $@
clean:
	rm -rf build
