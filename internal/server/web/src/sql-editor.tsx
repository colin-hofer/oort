import {acceptCompletion, autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap} from '@codemirror/autocomplete';
import {defaultKeymap, history, historyKeymap} from '@codemirror/commands';
import {sql, PostgreSQL} from '@codemirror/lang-sql';
import {bracketMatching, HighlightStyle, indentOnInput, syntaxHighlighting} from '@codemirror/language';
import {Compartment, EditorState} from '@codemirror/state';
import {drawSelection, EditorView, keymap} from '@codemirror/view';
import {tags} from '@lezer/highlight';
import {useEffect, useRef} from 'react';

// Table -> column names, fed to lang-sql so completions match the tenant's data.
export type SqlSchema = Record<string, string[]>;

const theme = EditorView.theme({
  '&': {backgroundColor: 'var(--well)', color: 'var(--ink)', fontSize: '12.5px'},
  '.cm-scroller': {fontFamily: 'var(--mono)', lineHeight: '1.6'},
  '.cm-content': {padding: '10px 12px', minHeight: '150px', caretColor: 'var(--gold)'},
  '&.cm-focused': {outline: 'none'},
  '.cm-cursor': {borderLeftColor: 'var(--gold)'},
  '.cm-selectionBackground, &.cm-focused .cm-selectionBackground': {backgroundColor: 'rgb(232 182 76 / 16%)'},
  '.cm-selectionMatch': {backgroundColor: 'rgb(232 182 76 / 10%)'},
  '.cm-matchingBracket, &.cm-focused .cm-matchingBracket': {backgroundColor: 'rgb(232 182 76 / 14%)', outline: 'none'},
  '.cm-tooltip': {backgroundColor: 'var(--raised)', border: '1px solid var(--line-strong)', color: 'var(--ink)'},
  '.cm-tooltip.cm-tooltip-autocomplete > ul': {fontFamily: 'var(--mono)', fontSize: '12px'},
  '.cm-tooltip-autocomplete ul li[aria-selected]': {backgroundColor: 'var(--gold)', color: 'var(--gold-ink)'},
  '.cm-completionMatchedText': {textDecoration: 'none', color: 'var(--gold-bright)'},
  '.cm-tooltip-autocomplete ul li[aria-selected] .cm-completionMatchedText': {color: 'inherit'},
  '.cm-completionIcon': {opacity: '0.6'},
}, {dark: true});

const highlight = HighlightStyle.define([
  {tag: tags.keyword, color: 'var(--gold)'},
  {tag: [tags.string, tags.special(tags.string)], color: 'var(--ok)'},
  {tag: [tags.number, tags.bool, tags.null], color: 'var(--gold-bright)'},
  {tag: tags.comment, color: 'var(--ink-faint)', fontStyle: 'italic'},
  {tag: [tags.operator, tags.punctuation], color: 'var(--ink-soft)'},
  {tag: [tags.typeName, tags.standard(tags.name)], color: 'var(--ink)'},
]);

const language = (schema: SqlSchema) => sql({dialect: PostgreSQL, schema, upperCaseKeywords: true});

export function SqlEditor({value, schema, onChange, onRun}: {
  value: string;
  schema: SqlSchema;
  onChange: (sql: string) => void;
  onRun: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const lang = useRef(new Compartment());
  const callbacks = useRef({onChange, onRun});
  callbacks.current = {onChange, onRun};

  useEffect(() => {
    if (!host.current) return;
    view.current = new EditorView({
      parent: host.current,
      state: EditorState.create({
        extensions: [
          keymap.of([
            {key: 'Mod-Enter', run: () => (callbacks.current.onRun(), true)},
            {key: 'Tab', run: acceptCompletion},
            ...closeBracketsKeymap,
            ...completionKeymap,
            ...defaultKeymap,
            ...historyKeymap,
          ]),
          history(),
          drawSelection(),
          indentOnInput(),
          bracketMatching(),
          closeBrackets(),
          autocompletion(),
          lang.current.of(language({})),
          syntaxHighlighting(highlight),
          theme,
          EditorView.lineWrapping,
          EditorView.contentAttributes.of({'aria-label': 'SQL query'}),
          EditorView.updateListener.of(update => {
            if (update.docChanged) callbacks.current.onChange(update.state.doc.toString());
          }),
        ],
      }),
    });
    return () => {
      view.current?.destroy();
      view.current = null;
    };
  }, []);

  // The parent owns the draft; push external changes (switching saved queries)
  // into the editor without disturbing local typing.
  useEffect(() => {
    const current = view.current;
    if (current && value !== current.state.doc.toString()) {
      current.dispatch({changes: {from: 0, to: current.state.doc.length, insert: value}});
    }
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({effects: lang.current.reconfigure(language(schema))});
  }, [schema]);

  return <div className="sql-editor" ref={host} />;
}
