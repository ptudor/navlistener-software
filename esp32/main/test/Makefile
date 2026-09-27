CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
INCLUDES := -Istubs -I../../components/journal/test/mocks -I../../components/netcfg/test/mocks -I../../components/ubx/include -I../../components/gnf1/include -I../../components/observer_report/include -I../../components/journal/include -I../../components/spool/include -I../../components/pusher/include
.PHONY: all clean
all: build/reception_runtime_test
	./build/reception_runtime_test
build/reception_runtime_test: reception_runtime_test.c ../reception_runtime.c ../reception_runtime.h ../../../common/reception.h ../../components/ubx/src/gnss_status.c
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) $< ../../components/ubx/src/gnss_status.c ../../components/gnf1/src/gnf1.c -lm -o $@
clean:
	rm -rf build
