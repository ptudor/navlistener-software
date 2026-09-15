CC ?= cc
CFLAGS ?= -std=c11 -Wall -Wextra -Werror -pedantic

TEST := hardware_manifest_policy_test
SRC := test_policy.c ../src/hardware_manifest_policy.c
INC := -I../include

.PHONY: all clean
all: $(TEST) build/checks_test
	./$(TEST)
	./build/checks_test

build/checks_test: checks_test.c ../src/hardware_checks.c ../include/hardware_checks.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INC) checks_test.c ../src/hardware_checks.c -o $@

$(TEST): $(SRC)
	$(CC) $(CFLAGS) $(INC) $(SRC) -o $@

clean:
	rm -f $(TEST)
	rm -rf build
