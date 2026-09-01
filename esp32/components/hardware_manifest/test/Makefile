CC ?= cc
CFLAGS ?= -std=c11 -Wall -Wextra -Werror -pedantic

TEST := hardware_manifest_policy_test
SRC := test_policy.c ../src/hardware_manifest_policy.c
INC := -I../include

.PHONY: all clean
all: $(TEST)
	./$(TEST)

$(TEST): $(SRC)
	$(CC) $(CFLAGS) $(INC) $(SRC) -o $@

clean:
	rm -f $(TEST)
