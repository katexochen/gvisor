load("//tools:defs.bzl", "go_library", "go_test", "proto_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

proto_library(
    name = "control",
    srcs = ["control.proto"],
    visibility = ["//visibility:public"],
    deps = [
        "@com_google_protobuf//:timestamp_proto",
    ],
)

go_library(
    name = "control",
    srcs = [
        "cgroups.go",
        "control.go",
        "events.go",
        "fs.go",
        "lifecycle.go",
        "logging.go",
        "metrics.go",
        "pprof.go",
        "proc.go",
        "state.go",
        "usage.go",
    ],
    visibility = [
        "//:sandbox",
    ],
    deps = [
        ":control_go_proto",
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/eventchannel",
        "//pkg/fd",
        "//pkg/fspath",
        "//pkg/log",
        "//pkg/metric",
        "//pkg/metric:metric_go_proto",
        "//pkg/prometheus",
        "//pkg/sentry/fdimport",
        "//pkg/sentry/fsimpl/host",
        "//pkg/sentry/fsimpl/user",
        "//pkg/sentry/fsmetric",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/ktime",
        "//pkg/sentry/limits",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state",
        "//pkg/sentry/strace",
        "//pkg/sentry/usage",
        "//pkg/sentry/vfs",
        "//pkg/sentry/watchdog",
        "//pkg/sync",
        "//pkg/tcpip/link/sniffer",
        "//pkg/urpc",
        "//pkg/usermem",
        "@org_golang_google_protobuf//types/known/timestamppb",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "control_test",
    size = "small",
    srcs = ["proc_test.go"],
    library = ":control",
    deps = [
        "//pkg/log",
        "//pkg/sentry/ktime",
        "//pkg/sentry/usage",
    ],
)
