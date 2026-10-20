idf_component_register(
    SRCS "src/sbf.c" "src/sbf_status.c" "src/sbf_control.c"
    INCLUDE_DIRS "include"
    REQUIRES ubx
)
