// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "Successbox",
    platforms: [.macOS("26.0")],
    products: [.executable(name: "successbox-apple", targets: ["SuccessboxApple"])],
    dependencies: [
        .package(url: "https://github.com/apple/containerization.git", exact: "0.53.0"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", exact: "2.4.2"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", exact: "2.9.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", exact: "2.4.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", exact: "1.38.1"),
        .package(url: "https://github.com/apple/swift-argument-parser.git", exact: "1.8.2"),
    ],
    targets: [
        .target(
            name: "SandboxProtocol",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            path: "api/sandbox/v1",
            exclude: ["sandbox.pb.go", "sandbox_grpc.pb.go"],
            plugins: [.plugin(name: "GRPCProtobufGenerator", package: "grpc-swift-protobuf")]
        ),
        .executableTarget(
            name: "SuccessboxApple",
            dependencies: [
                "SandboxProtocol",
                .product(name: "Containerization", package: "containerization"),
                .product(name: "ContainerizationEXT4", package: "containerization"),
                .product(name: "ContainerizationOS", package: "containerization"),
                .product(name: "ContainerizationOCI", package: "containerization"),
                .product(name: "ContainerizationIO", package: "containerization"),
                .product(name: "ContainerizationExtras", package: "containerization"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
            ],
            path: "apple/Sources/SuccessboxApple"
        ),
    ]
)
