#!/usr/bin/env python3
"""Export saved board placements and their bound STEP models for offline rendering.

Run with a Python that provides FreeCAD, Part and pycryptodome. Hardware inputs
are read only. Scene files contain design geometry and must stay out of Git.
"""
import argparse
import ast
import hashlib
import json
import math
import sys
from pathlib import Path

try:
    import FreeCAD as App
except ImportError:
    import freecad  # Some installations expose a module-path bootstrap.
    import FreeCAD as App
import Part

MM = 0.0254


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def mesh(shape, colour):
    vertices, faces = shape.tessellate(0.04)
    return {'colour': colour,
            'vertices': [round(v, 5) for p in vertices for v in (p.x, p.y, p.z)],
            'faces': [v for f in faces for v in f]}


def rounded_rect(width, height, radius=0):
    radius = min(radius, width / 2, height / 2)
    points = []
    if radius:
        for x, y, start in [(width/2-radius, height/2-radius, 0),
                            (-width/2+radius, height/2-radius, 90),
                            (-width/2+radius, -height/2+radius, 180),
                            (width/2-radius, -height/2+radius, 270)]:
            for i in range(13):
                a = math.radians(start + i * 90/12)
                points.append(App.Vector(x + radius*math.cos(a), y + radius*math.sin(a), 0))
    else:
        points = [App.Vector(x*width/2, y*height/2, 0) for x, y in [(1,1),(-1,1),(-1,-1),(1,-1)]]
    return Part.Face(Part.Wire(Part.makePolygon(points + [points[0]]).Edges))


def pad_face(spec):
    kind = spec['padType']
    if kind == 'POLYGON':
        p = spec['path']
        if p and isinstance(p[0], list):
            if len(p) != 1:
                raise ValueError('Unsupported multi-path pad')
            p = p[0]
        if any(isinstance(v, str) and v != 'L' for v in p):
            raise ValueError('Unsupported polygon pad command')
        p = [v for v in p if v != 'L']
        pts = [App.Vector(p[i]*MM, p[i+1]*MM, 0) for i in range(0, len(p), 2)]
        if (pts[0]-pts[-1]).Length > 1e-6:
            pts.append(pts[0])
        return Part.Face(Part.Wire(Part.makePolygon(pts).Edges))
    w, h = spec['width']*MM, spec['height']*MM
    if kind == 'ELLIPSE':
        pts = [App.Vector(w/2*math.cos(i*math.tau/48), h/2*math.sin(i*math.tau/48), 0) for i in range(48)]
        return Part.Face(Part.Wire(Part.makePolygon(pts + [pts[0]]).Edges))
    if kind not in ('RECT', 'OVAL'):
        raise ValueError(f'Unsupported pad type {kind}')
    return rounded_rect(w, h, min(w,h)/2 if kind == 'OVAL' else min(w,h)*spec.get('radius',0)/200)


def placed(shape, component):
    shape = shape.copy()
    if component.get('layerId') == 2:
        shape.rotate(App.Vector(), App.Vector(0,1,0), 180)
    shape.rotate(App.Vector(), App.Vector(0,0,1), component.get('angle',0))
    shape.translate(App.Vector(component.get('x',0)*MM, component.get('y',0)*MM, 0))
    return shape


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--hardware', required=True, type=Path)
    parser.add_argument('--materials', required=True, type=Path, help='TUDOR model library tudor_step.py')
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    sys.path.insert(0, str(args.hardware / 'pcb/tools'))
    from easyeda_saved import read_project, attributes
    from model_binding import expected, matches

    # Read the model library palette without executing its export code.
    palette = next(ast.literal_eval(n.value) for n in ast.parse(args.materials.read_text()).body
                   if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == 'COLOURS' for t in n.targets))
    config_path = args.hardware / 'pcb/previews/boards.json'
    config = json.loads(config_path.read_text())
    models_dir = args.hardware / 'pcb/models3d'
    args.output.mkdir(parents=True, exist_ok=True)
    models, model_hashes, boards = {}, {}, []

    for spec in config['boards']:
        path = (config_path.parent / spec['project']).resolve()
        source_hash = digest(path)
        _, docs = read_project(path)
        rows = list(docs[spec['pcb']]['records'].values())
        attrs = attributes(rows)
        outlines = [r['path'] for r in rows if r['type'] == 'POLY' and r.get('layerId') == 11]
        if len(outlines) != 1 or outlines[0][0] != 'R' or outlines[0][5] != 0:
            raise ValueError('Expected one unrotated rounded rectangular board')
        _, bx, by, width, height, _, *radius = outlines[0]
        physical = [r for r in rows if r['type'] == 'LAYER_PHYS']
        thickness = sum(r.get('thickness',0) or 0 for r in physical)*MM
        if not 0.5 < thickness < 3:
            raise ValueError(f'Unexpected board thickness {thickness}')
        pcb = rounded_rect(width*MM, height*MM, (radius[0] if radius else 0)*MM).extrude(App.Vector(0,0,thickness))
        pcb.translate(App.Vector((bx+width/2)*MM, (by-height/2)*MM, -thickness/2))
        pads, drills, instances, omitted = [], [], [], []

        def add_drill_fill(item, component):
            paths = item['path'] if isinstance(item['path'][0],list) else [item['path']]
            for p in paths:
                if p[0] != 'CIRCLE':
                    raise ValueError('Unsupported unplated drill')
                hole = Part.makeCylinder(p[3]*MM,thickness*3,App.Vector(p[1]*MM,p[2]*MM,-thickness*1.5))
                drills.append(placed(hole,component))

        def add_pad(row, component):
            layer = row.get('layerId')
            if component.get('layerId') == 2 and layer in (1,2):
                layer = 3-layer
            if layer not in (1,2,12):
                return
            f = pad_face(row['defaultPad'])
            if row['defaultPad']['padType'] != 'POLYGON':
                f.rotate(App.Vector(), App.Vector(0,0,1), row.get('padAngle',0))
                f.translate(App.Vector(row['centerX']*MM,row['centerY']*MM,0))
            f = placed(f, component)
            hole = row.get('hole')
            drill = None
            if hole and hole['width'] > 0:
                h = pad_face(dict(padType='ELLIPSE' if hole['holeType']=='ROUND' else 'OVAL',
                                  width=hole['width'], height=hole['height']))
                h.rotate(App.Vector(),App.Vector(0,0,1),row.get('relativeAngle',0) or 0)
                h.translate(App.Vector(-row.get('padOffsetX',0)*MM,-row.get('padOffsetY',0)*MM,0))
                h.rotate(App.Vector(),App.Vector(0,0,1),row.get('padAngle',0))
                h.translate(App.Vector(row['centerX']*MM,row['centerY']*MM,0))
                h = placed(h,component)
                h.translate(App.Vector(0,0,-thickness))
                drill = h.extrude(App.Vector(0,0,thickness*2))
                drills.append(drill)
            if row.get('plated') is False:
                return
            for side in ([1,2] if layer == 12 else [layer]):
                land = f.copy().extrude(App.Vector(0,0,0.025))
                land.translate(App.Vector(0,0,thickness/2+0.008 if side == 1 else -thickness/2-0.033))
                if drill:
                    land = land.cut(drill)
                if not land.isNull():
                    pads.append(land)

        for row in rows:
            if row['type'] == 'PAD':
                add_pad(row,{})
            if row['type'] == 'FILL' and row.get('layerId') == 12:
                add_drill_fill(row,{})
            if row['type'] != 'COMPONENT':
                continue
            a = {**row.get('attrs',{}), **attrs.get(row['id'],{})}
            ref = a.get('Designator','')
            fp = docs[a['Footprint']]['records'].values()
            for item in fp:
                if item['type'] == 'PAD':
                    add_pad(item,row)
                if item['type'] == 'FILL' and item.get('layerId') == 12:
                    add_drill_fill(item,row)
            binding = a.get('3D Model')
            if not binding:
                if not (ref.startswith('TP') or 'DNP' in a.get('Name','')):
                    raise ValueError(f'{spec["id"]} {ref}: missing model')
                omitted.append(ref)
                continue
            name = binding.split('.step|')[0]
            if not matches(a,expected(name,'models3d')):
                raise ValueError(f'{spec["id"]} {ref}: model pose differs from its receipt')
            if name not in models:
                receipt = json.loads((models_dir/f'{name}.json').read_text())
                step = models_dir/f'{name}.step'
                model_hashes[name] = digest(step)
                if model_hashes[name] != receipt['files'][step.name]['sha256']:
                    raise ValueError(f'{name}: STEP digest differs from its receipt')
                shape = Part.Shape()
                shape.read(str(step))
                colours = [p[1] for p in receipt['parts'] for _ in range(p[2] if len(p)>2 else 1)]
                if len(shape.Solids) != len(colours):
                    raise ValueError(f'{name}: solid and colour counts differ')
                groups = {}
                for solid, colour in zip(shape.Solids,colours):
                    groups.setdefault(colour,[]).append(solid)
                models[name] = [mesh(Part.makeCompound(solids),palette[colour]) for colour,solids in groups.items()]
            instances.append({'model':name,'x':row['x']*MM,'y':row['y']*MM,
                              'angle':row.get('angle',0),'bottom':row['layerId']==2})
        print(f'{spec["id"]}: {len(instances)} components, {len(drills)} drills',flush=True)
        if drills:
            pcb = pcb.cut(Part.makeCompound(drills))
        scene = {'id':spec['id'],'thickness':thickness,
                 'board':[mesh(pcb,[0.055,0.32,0.13]),mesh(Part.makeCompound(pads),[0.72,0.65,0.43])],
                 'instances':instances}
        (args.output/f'{spec["id"]}.json').write_text(json.dumps(scene,separators=(',',':')))
        if source_hash != digest(path):
            raise ValueError('Project changed during export; rerun against a saved snapshot')
        boards.append({'id':spec['id'],'title':spec['title'],'width_mm':round(width*MM,4),
                       'height_mm':round(height*MM,4),'source_sha256':source_hash,
                       'component_count':len(instances),'unfitted_or_test_points':omitted})
    (args.output/'models.json').write_text(json.dumps(models,separators=(',',':')))
    manifest = {'version':2,'kind':'3D render','finish':'green solder mask; no silkscreen',
                'width_pixels':1600,'height_pixels':1200,'boards':boards,
                'model_set_sha256':hashlib.sha256(json.dumps(model_hashes,sort_keys=True).encode()).hexdigest(),
                'files':{}}
    (args.output/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')


if __name__ == '__main__':
    main()
