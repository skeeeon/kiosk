// A small, kiosk-local Leaflet wrapper for the Locations map view
// (docs/location-sightings-plan.md, L4). Deliberately trimmed to what the
// Locations view needs: clustered dots + coarse-area halos over a dark
// basemap. No dynamic/KV-driven markers, no theme switching, no JSONPath — the
// SPA is fixed-dark and the data is a static per-load snapshot.
//
// Markers are vector `L.divIcon` dots (a styled <div>, no image asset) rather
// than Leaflet's default PNG pin, so the map ships no marker images and needs
// no CDN/icon-bundling shim — only the basemap tiles hit the network, which is
// inherent to any web map. Each dot is paired with an `L.circle` halo: the dot
// marks the last-seen point, the halo renders the *coarse area* the sighting
// actually implies (a gateway fix locates the tag to within its read range,
// not to a survey-grade point). Halo radius is real meters, so it grows/shrinks
// with zoom — sub-pixel at fleet zoom (clustering takes over), legible at street
// zoom exactly when someone would otherwise read the dot literally.
import { shallowRef, onUnmounted } from 'vue'
import L from 'leaflet'
import 'leaflet/dist/leaflet.css'
// maplibre-gl is held at v5 on purpose. v5 inlines its tile-parsing worker into
// dist/maplibre-gl.js; v6 splits it out and resolves it as a sibling file
// (`new URL('./maplibre-gl-worker.mjs', import.meta.url)`) that Vite never emits
// once the library is bundled into a hashed chunk. Nothing throws and nothing
// reaches the console — the style still loads and its background layer still
// paints, so water, landuse, roads and labels vanish together. On a fixed-dark
// SPA that failure is near-invisible: it reads as a plain dark panel with dots
// on it. v5 is also the version OpenFreeMap's own quick start pins.
import 'maplibre-gl/dist/maplibre-gl.css'
// Side-effect import: registers L.maplibreGL and augments the leaflet module types.
import '@maplibre/maplibre-gl-leaflet'
import 'leaflet.markercluster'
import 'leaflet.markercluster/dist/MarkerCluster.css'
import 'leaflet.markercluster/dist/MarkerCluster.Default.css'

// OpenFreeMap's fiord: a designed dark style that matches the SPA's fixed slate
// palette. Keyless, uncapped, commercial use permitted, self-hostable.
//
// This replaced CARTO's dark_all, which was not a preference but a forced move —
// CARTO put its raster basemaps behind an API key and is retiring them, and this
// SPA is fixed-dark, so dark_all was the only basemap here with nothing to fall
// back to. Lifted from the platform, which moved first.
//
// It is a MapLibre style document, not a {z}/{x}/{y} template (OpenFreeMap
// publishes no raster endpoint), so it renders through L.maplibreGL onto a WebGL
// canvas instead of L.tileLayer. Everything drawn on top is unchanged Leaflet:
// the divIcon dots, their L.circle halos and the cluster group are all overlay
// panes above the GL canvas. It does mean the basemap needs WebGL — worth
// remembering, since this SPA also runs on the mini-PC appliance.
const STYLE_URL = 'https://tiles.openfreemap.org/styles/fiord'
// The style JSON carries no `attribution` on its sources, so MapLibre renders no
// credit of its own and ODbL still requires one. It goes on the map's attribution
// control rather than the layer: L.maplibreGL's options are typed as MapLibre's
// own, which have no Leaflet `attribution` key.
const TILE_ATTRIBUTION =
  '&copy; <a href="https://openfreemap.org">OpenFreeMap</a> &middot; <a href="https://www.openmaptiles.org/">OpenMapTiles</a> &middot; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>'

// US centroid — the neutral default before markers are fit. The view calls
// fitAllMarkers() on entry, so this is only ever seen when there are no pins.
const DEFAULT_CENTER: L.LatLngExpression = [39.8283, -98.5795]
const DEFAULT_ZOOM = 4

export interface MapMarkerInput {
  id: string
  lat: number
  lon: number
  label?: string
  // Hex color driven by sighting staleness (fresh → old). Applied to both the
  // dot and its halo so temporal trust reads at a glance.
  color: string
  // Coarse-area radius in meters — the spatial "somewhere around here" the
  // gateway fix implies.
  radiusM: number
}

export interface InitMapOptions {
  // When set, cluster clicks route here instead of Leaflet's default
  // zoom/spiderfy. The handler receives the child marker ids. We still zoom to
  // fit a cluster whose points CAN be separated by zooming; the list is only
  // surfaced for the un-separable case (identical coordinates, or already at
  // max zoom) — which is the norm for our coarse gateway fixes and is exactly
  // where a 20-leg spiderfy spiral falls apart.
  onClusterClick?: (ids: string[]) => void
}

export function useLeafletMap() {
  const map = shallowRef<L.Map | null>(null)
  const clusterLayer = shallowRef<L.MarkerClusterGroup | null>(null)
  const haloLayer = shallowRef<L.FeatureGroup | null>(null)

  // Idempotent: the view lazily inits on first switch to the map tab, and a
  // re-entry just re-fits. A second init on a live map would leak the old one.
  function initMap(containerId: string, opts: InitMapOptions = {}) {
    if (map.value) return
    const container = document.getElementById(containerId)
    if (!container) return

    const m = L.map(containerId, {
      center: DEFAULT_CENTER,
      zoom: DEFAULT_ZOOM,
      // Was an L.tileLayer option; the GL layer supplies no zoom bounds, and
      // without this Leaflet would zoom in without limit (and the cluster-click
      // handler's getMaxZoom() check would never fire).
      maxZoom: 19,
      zoomControl: false,
      attributionControl: true,
    })
    // Leaflet's default prefix is width the credit needs, and the credit that
    // has to be there is the data's, not the library's.
    m.attributionControl.setPrefix(false)
    m.attributionControl.addAttribution(TILE_ATTRIBUTION)
    L.control.zoom({ position: 'bottomleft' }).addTo(m)
    // attributionControl: false — the credit lives on the Leaflet control above,
    // not on a second one MapLibre would draw itself.
    L.maplibreGL({ style: STYLE_URL, attributionControl: false }).addTo(m)

    const onClusterClick = opts.onClusterClick
    const cluster = L.markerClusterGroup({
      chunkedLoading: true,
      maxClusterRadius: 50,
      showCoverageOnHover: false,
      // With a custom handler we own the click entirely (see below); disable
      // the built-in zoom + spiral so they don't fire alongside it.
      zoomToBoundsOnClick: !onClusterClick,
      spiderfyOnMaxZoom: !onClusterClick,
    })
    if (onClusterClick) {
      cluster.on('clusterclick', (e: any) => {
        const layer = e.layer
        const bounds = layer.getBounds()
        // Degenerate bounds = every child at (near-)identical coordinates, so
        // zooming can never separate them. Otherwise, if we're not yet at max
        // zoom, drill in — fitBounds has no maxZoom cap, so a tight cluster
        // eventually reaches max zoom and then falls through to the list.
        const degenerate = bounds.getNorthEast().equals(bounds.getSouthWest(), 1e-6)
        if (!degenerate && m.getZoom() < m.getMaxZoom()) {
          m.fitBounds(bounds, { padding: [40, 40] })
          return
        }
        const ids = (layer.getAllChildMarkers() as L.Marker[])
          .map((mk) => (mk as any).__rrId as string | undefined)
          .filter((id): id is string => !!id)
        onClusterClick(ids)
      })
    }
    cluster.addTo(m)
    // Halos live outside the cluster group (cluster plugin only clusters
    // Markers, not vector layers) so they render independently at every zoom.
    const halos = L.featureGroup().addTo(m)

    map.value = m
    clusterLayer.value = cluster
    haloLayer.value = halos

    // A freshly-shown container can report zero size for a frame; nudge Leaflet
    // once it's laid out so the basemap fills the pane.
    setTimeout(() => m.invalidateSize(), 100)
  }

  // A colored dot with a dark ring, styled entirely inline so it needs no CSS
  // (overriding the default `leaflet-div-icon` white box via a fresh class).
  function dotIcon(color: string): L.DivIcon {
    return L.divIcon({
      className: 'kiosk-map-dot',
      html: `<span style="display:block;width:12px;height:12px;border-radius:9999px;background:${color};border:2px solid rgba(15,23,42,0.9);box-shadow:0 0 0 1px ${color}80"></span>`,
      iconSize: [16, 16],
      iconAnchor: [8, 8],
    })
  }

  function renderMarkers(markers: MapMarkerInput[], onClick?: (id: string) => void) {
    const cluster = clusterLayer.value
    const halos = haloLayer.value
    if (!cluster || !halos) return

    cluster.clearLayers()
    halos.clearLayers()

    markers.forEach(({ id, lat, lon, label, color, radiusM }) => {
      L.circle([lat, lon], {
        radius: radiusM,
        color,
        weight: 1,
        opacity: 0.45,
        fillColor: color,
        fillOpacity: 0.12,
        interactive: false, // clicks belong to the dot, not the halo
      }).addTo(halos)

      const marker = L.marker([lat, lon], { icon: dotIcon(color), title: label })
      ;(marker as any).__rrId = id // read back by the cluster-click handler
      if (onClick) marker.on('click', () => onClick(id))
      if (label) marker.bindTooltip(label, { direction: 'top', offset: [0, -8] })
      cluster.addLayer(marker)
    })
  }

  // Fit the viewport to all dots. No-op (returns false) when there are none, so
  // the caller keeps the default center rather than fitting empty bounds.
  function fitAllMarkers(): boolean {
    const m = map.value
    const cluster = clusterLayer.value
    if (!m || !cluster) return false
    const bounds = cluster.getBounds()
    if (!bounds.isValid()) return false
    m.fitBounds(bounds, { padding: [40, 40], maxZoom: 16 })
    return true
  }

  function invalidateSize() {
    map.value?.invalidateSize()
  }

  function cleanup() {
    if (map.value) {
      map.value.remove()
      map.value = null
    }
    clusterLayer.value = null
    haloLayer.value = null
  }

  onUnmounted(cleanup)

  return { initMap, renderMarkers, fitAllMarkers, invalidateSize, cleanup }
}
