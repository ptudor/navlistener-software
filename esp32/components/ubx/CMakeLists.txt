idf_component_register(
    SRCS "src/ubx.c" "src/ubx_probe.c" "src/gnss_status.c"
    INCLUDE_DIRS "include"
    REQUIRES gnf1
)
