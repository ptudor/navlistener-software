CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
INCLUDES := -I../include -I../../ubx/include
CANVAS := ../src/oled_canvas.c ../src/oled_font.c
HEADERS := ../include/oled_canvas.h ../include/oled_pages.h ../include/oled_gnss.h ../include/ssd1306.h
.PHONY: all clean
all: build/canvas_test build/ssd1306_test build/pages_test
	./build/canvas_test
	./build/ssd1306_test
	./build/pages_test build
build/canvas_test: canvas_test.c $(CANVAS) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) canvas_test.c $(CANVAS) -o $@
build/ssd1306_test: ssd1306_test.c ../src/ssd1306.c $(CANVAS) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) ssd1306_test.c ../src/ssd1306.c $(CANVAS) -o $@
# pages_test writes each screen it draws to build/<name>.pbm, for a look at the layout.
build/pages_test: pages_test.c ../src/oled_pages.c ../src/oled_gnss.c ../../ubx/src/gnss_status.c $(CANVAS) $(HEADERS) ../../ubx/include/gnss_status.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) pages_test.c ../src/oled_pages.c ../src/oled_gnss.c ../../ubx/src/gnss_status.c $(CANVAS) -lm -o $@
clean:
	rm -rf build
