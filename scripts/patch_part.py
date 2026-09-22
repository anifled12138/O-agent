with open('frontend/app/OApp.tsx', 'r', encoding='utf-8') as f:
    content = f.read()

def replace_chunk(source, target_old, target_new):
    norm_source = source.replace('\r\n', '\n')
    norm_old = target_old.replace('\r\n', '\n')
    norm_new = target_new.replace('\r\n', '\n')
    if norm_old not in norm_source:
        raise Exception('Target not found: ' + norm_old[:60])
    res = norm_source.replace(norm_old, norm_new)
    if '\r\n' in source:
        res = res.replace('\n', '\r\n')
    return res
