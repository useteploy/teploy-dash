// Test double for the confirm dialog store: routes askConfirm/askForm to the
// test's globalThis.confirm / globalThis.prompt stubs, read at call time, so
// a test answers the dialog the way it used to answer the browser one.
// confirm gets "title\n\nmessage"; a typed-name dialog passes when prompt
// returns the name; a form's first field takes prompt's answer.
export function bridgeDialogs(stores) {
  const dialog = stores.dialog;
  if (!dialog) return;
  dialog.opened = [];
  dialog.open = (o) => {
    dialog.opened.push(o);
    const text = o.title + (o.message ? '\n\n' + o.message : '');
    const fields = o.fields || [];
    if (o.typed && typeof globalThis.prompt === 'function' && !fields.length) {
      const typed = globalThis.prompt(text, '');
      return Promise.resolve(o.kind === 'form' ? (typed === o.typed ? {} : null) : typed === o.typed);
    }
    if (o.kind === 'form' && typeof globalThis.prompt === 'function' && fields.length) {
      const value = globalThis.prompt(text, fields[0].value ?? '');
      if (value === null || value === undefined) return Promise.resolve(null);
      const values = Object.fromEntries(fields.map((f) => [f.key, f.value ?? '']));
      values[fields[0].key] = value;
      return Promise.resolve(values);
    }
    const ok = typeof globalThis.confirm === 'function' ? !!globalThis.confirm(text) : true;
    if (o.kind === 'form') return Promise.resolve(ok ? Object.fromEntries(fields.map((f) => [f.key, f.value ?? ''])) : null);
    return Promise.resolve(ok);
  };
}
