import AppKit

// Read the canonical MemQL SVG geometry. The original mark is bundled,
// rather than approximating it with a system glyph or redrawing a variant.
final class MarkParser: NSObject, XMLParserDelegate {
    var edges: [(CGFloat, CGFloat, CGFloat, CGFloat)] = []
    var nodes: [(CGFloat, CGFloat, CGFloat)] = []
    func parser(_ parser: XMLParser, didStartElement name: String, namespaceURI: String?, qualifiedName: String?, attributes: [String: String]) {
        func n(_ key: String) -> CGFloat { CGFloat(Double(attributes[key] ?? "0") ?? 0) }
        if name == "line" { edges.append((n("x1"), n("y1"), n("x2"), n("y2"))) }
        if name == "circle" { nodes.append((n("cx"), n("cy"), n("r"))) }
    }
    static func image(size: CGFloat = 20, template: Bool = true, resourceURL: URL? = nil) -> NSImage? {
        guard let url = resourceURL ?? Bundle.main.url(forResource: "mark", withExtension: "svg"),
              let parser = XMLParser(contentsOf: url) else { return nil }
        let shape = MarkParser()
        parser.delegate = shape
        guard parser.parse(), shape.nodes.count == 9 else { return nil }
        let image = NSImage(size: NSSize(width: size, height: size), flipped: true) { rect in
            let accent = NSColor(name: nil) { appearance in
                appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                    ? NSColor(srgbRed: 92/255, green: 205/255, blue: 167/255, alpha: 1)
                    : NSColor(srgbRed: 4/255, green: 125/255, blue: 90/255, alpha: 1)
            }
            (template ? NSColor.black : accent).set()
            let scale = rect.width / 24
            let lines = NSBezierPath()
            lines.lineWidth = 0.78 * scale
            lines.lineCapStyle = .round
            for (x1, y1, x2, y2) in shape.edges {
                lines.move(to: NSPoint(x: x1 * scale, y: y1 * scale))
                lines.line(to: NSPoint(x: x2 * scale, y: y2 * scale))
            }
            lines.stroke()
            for (x, y, r) in shape.nodes {
                NSBezierPath(ovalIn: NSRect(x: (x-r)*scale, y: (y-r)*scale, width: 2*r*scale, height: 2*r*scale)).fill()
            }
            return true
        }
        image.isTemplate = template
        return image
    }
}
