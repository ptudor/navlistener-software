CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
INCLUDES := -I.. -Istubs -I../../components/journal/test/mocks -I../../components/netcfg/test/mocks -I../../components/ubx/include -I../../components/gnf1/include -I../../components/observer_report/include -I../../components/journal/include -I../../components/spool/include -I../../components/pusher/include
.PHONY: all clean
all: build/reception_runtime_test build/power_history_test
	./build/reception_runtime_test
	./build/power_history_test
build/reception_runtime_test: reception_runtime_test.c ../reception_runtime.c ../reception_runtime.h ../power_history.c ../power_history.h ../../../common/reception.h ../../../common/reception_power.h ../../components/ubx/src/gnss_status.c
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) $< ../power_history.c ../../components/ubx/src/gnss_status.c ../../components/gnf1/src/gnf1.c -lm -o $@
build/power_history_test: power_history_test.c ../power_history.c ../power_history.h ../../../common/reception_power.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) $< ../power_history.c -lm -o $@
clean:
	rm -rf build
