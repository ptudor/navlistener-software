# regulator_thermal — the Wi-Fi throttle's policy (include/regulator_thermal.h). Pure logic;
# the board task reads the sensor and applies the transmit cap.
idf_component_register(
    SRCS "src/regulator_thermal.c"
    INCLUDE_DIRS "include"
)
