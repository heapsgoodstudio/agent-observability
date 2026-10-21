// Build-time only: draws the app icon into an .iconset for iconutil, then is
// discarded. Keeps the repo free of binary assets.
//
// The mark: a near-black tile carrying Claude's twelve-spoke burst in Claude
// orange. This is the only copy of the geometry; the frontend has no icon of
// its own yet, and whoever gives it one should match this rather than
// re-derive it. Note SVG y grows downward while CoreGraphics y grows upward.
//
// Geometry is expressed in the SVG's 64 unit box and scaled per size. Each
// spoke is a round-capped stroke run from the centre to `radius - width / 2`,
// so the cap carries the tip out to `radius`. The angles and widths are
// deliberately uneven, as the mark itself is. The two coordinate systems
// disagree on direction: SVG y grows downward, CoreGraphics y grows upward, so
// every angle below is negated before it is drawn.
//
// Usage: icon <output.iconset directory>
import AppKit

let BOX: CGFloat = 64
let TILE_RADIUS: CGFloat = 14
let CENTRE: CGFloat = 32

func rgb(_ hex: UInt32) -> NSColor {
    NSColor(srgbRed: CGFloat((hex >> 16) & 0xff) / 255,
            green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: 1)
}

let TILE = rgb(0x141413)
let MARK = rgb(0xd97757)
let SPOKES: [(angle: CGFloat, radius: CGFloat, width: CGFloat)] = [
    (9.2, 21.8, 3.2),
    (41.0, 21.6, 2.5),
    (59.5, 20.6, 3.9),
    (97.0, 21.7, 3.2),
    (128.0, 22.3, 2.9),
    (151.5, 22.0, 3.3),
    (185.0, 24.4, 2.4),
    (216.5, 25.2, 4.3),
    (243.0, 27.0, 4.9),
    (276.2, 23.7, 3.5),
    (306.5, 23.4, 5.6),
    (346.8, 21.9, 3.4),
]

func render(_ pixels: Int) -> Data {
    guard let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)
    else { fatalError("could not allocate a \(pixels)px bitmap") }

    NSGraphicsContext.saveGraphicsState()
    defer { NSGraphicsContext.restoreGraphicsState() }
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)

    let s = CGFloat(pixels) / BOX
    TILE.setFill()
    NSBezierPath(roundedRect: NSRect(x: 0, y: 0, width: CGFloat(pixels), height: CGFloat(pixels)),
                 xRadius: TILE_RADIUS * s, yRadius: TILE_RADIUS * s).fill()
    MARK.setStroke()
    for spoke in SPOKES {
        let a = -spoke.angle * .pi / 180 // SVG y grows down, CoreGraphics up
        let reach = spoke.radius - spoke.width / 2
        let path = NSBezierPath()
        path.lineWidth = spoke.width * s
        path.lineCapStyle = .round
        path.move(to: NSPoint(x: CENTRE * s, y: CENTRE * s))
        path.line(to: NSPoint(x: (CENTRE + reach * cos(a)) * s,
                              y: (CENTRE + reach * sin(a)) * s))
        path.stroke()
    }

    guard let png = rep.representation(using: .png, properties: [:]) else {
        fatalError("could not encode the \(pixels)px bitmap as PNG")
    }
    return png
}

guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write("usage: icon <output.iconset>\n".data(using: .utf8)!)
    exit(2)
}
let outDir = URL(fileURLWithPath: CommandLine.arguments[1])
try FileManager.default.createDirectory(at: outDir, withIntermediateDirectories: true)

// The exact set iconutil expects for a complete .icns.
for base in [16, 32, 128, 256, 512] {
    try render(base).write(to: outDir.appendingPathComponent("icon_\(base)x\(base).png"))
    try render(base * 2).write(to: outDir.appendingPathComponent("icon_\(base)x\(base)@2x.png"))
}
