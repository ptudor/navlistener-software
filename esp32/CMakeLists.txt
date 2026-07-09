# navfeeder-esp — the navlistener ESP32 edge feeder (top-level ESP-IDF project).
# Native ESP-IDF v5.4+ (no Arduino-as-component). Build with build-navfeeder-esp.sh,
# or: . ~/esp/esp-idf/export.sh && idf.py set-target esp32c6 && idf.py build
cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_SOURCE_DIR}/components")

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(navfeeder-esp)
