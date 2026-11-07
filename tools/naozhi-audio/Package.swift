// swift-tools-version: 6.2
// naozhi-audio: the meeting-audio helper (docs/rfc/meeting-notes.md §3.1). Build with build.sh,
// which links Info.plist into the binary (TCC reads the usage descriptions from it).
import Foundation
import PackageDescription

let infoPlist = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("Info.plist").path

let package = Package(
    name: "naozhi-audio",
    platforms: [.macOS("14.2")],
    dependencies: [
        // Speaker separation (§3.5): the CoreML port of pyannote community-1. Only the Diarizer
        // trait: no TTS, no text normalisation.
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.18.2", traits: ["Diarizer"]),
    ],
    targets: [
        .executableTarget(
            name: "naozhi-audio",
            dependencies: [.product(name: "FluidAudio", package: "FluidAudio")],
            path: "Sources/naozhi-audio",
            swiftSettings: [.swiftLanguageMode(.v5)],
            linkerSettings: [.unsafeFlags(["-Xlinker", "-sectcreate", "-Xlinker", "__TEXT", "-Xlinker", "__info_plist", "-Xlinker", infoPlist])]
        ),
    ]
)
