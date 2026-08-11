import Foundation
import Testing
@testable import IntegrityStation

@Test
func skyPlotIsNorthUpAndClockwise() throws {
    let north = try #require(SkyPlot.project(azimuthDegrees: 0, elevationDegrees: 0))
    let east = try #require(SkyPlot.project(azimuthDegrees: 90, elevationDegrees: 0))
    let south = try #require(SkyPlot.project(azimuthDegrees: 180, elevationDegrees: 0))
    let west = try #require(SkyPlot.project(azimuthDegrees: 270, elevationDegrees: 0))

    #expect(abs(north.x) < 1e-12 && abs(north.y + 1) < 1e-12)
    #expect(abs(east.x - 1) < 1e-12 && abs(east.y) < 1e-12)
    #expect(abs(south.x) < 1e-12 && abs(south.y - 1) < 1e-12)
    #expect(abs(west.x + 1) < 1e-12 && abs(west.y) < 1e-12)
}

@Test
func skyPlotPlacesZenithAtCenterAndRejectsInvalidElevation() throws {
    let zenith = try #require(SkyPlot.project(azimuthDegrees: 217, elevationDegrees: 90))
    #expect(abs(zenith.x) < 1e-12)
    #expect(abs(zenith.y) < 1e-12)
    #expect(SkyPlot.project(azimuthDegrees: 0, elevationDegrees: -1) == nil)
    #expect(SkyPlot.project(azimuthDegrees: 0, elevationDegrees: 91) == nil)
}
