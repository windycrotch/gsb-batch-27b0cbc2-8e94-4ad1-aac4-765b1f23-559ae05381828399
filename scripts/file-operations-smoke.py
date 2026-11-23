#!/usr/bin/env python3
"""Real filesystem changes and editor handoff through a PTY, isolated fixtures."""
import os
import shutil
from tui_session import Session, ROOT
folder=ROOT/'tmp/file-operation-fixtures'
if folder.exists():shutil.rmtree(folder)
folder.mkdir(parents=True)
(folder/'original.txt').write_text('Original contents\n')
(folder/'keep.txt').write_text('Do not overwrite\n')
(folder/'destination').mkdir()
editor=folder/'test-editor'
editor.write_text('#!/bin/sh\nprintf "Edited by fixture\\n" >> "$1"\nprintf "EDITOR_HANDOFF_OK\\n"\n')
editor.chmod(0o700)
s=None
try:
 s=Session('file-operations',str(folder/'original.txt'),env_override={'VISUAL':str(editor)})
 s.expect(b'Original contents')
 s.send(b'\x12');s.expect(b'RENAME');s.send(b'\x15keep.txt\r');s.expect(b'file exists')
 assert (folder/'original.txt').exists() and (folder/'keep.txt').read_text()=='Do not overwrite\n'
 s.send(b'\x1b');s.read(.2)
 s.send(b'\x12');s.expect(b'RENAME');s.send(b'\x15renamed file.txt\r');s.expect(b'rename complete')
 assert not (folder/'original.txt').exists() and (folder/'renamed file.txt').exists()
 s.send(b'e');s.expect(b'Editor closed');s.read(.3)
 assert b'EDITOR_HANDOFF_OK' in s.log
 assert 'Edited by fixture' in (folder/'renamed file.txt').read_text()
 s.send(b'M');s.expect(b'MOVE');s.send(b'destination\r');s.expect(b'move complete')
 assert not (folder/'renamed file.txt').exists() and (folder/'destination/renamed file.txt').exists()
 # Search into the destination, then select the file to delete.
 s.send(b'/destination');s.expect(b'Filter: destination');s.send(b'\r');s.read(.2);s.send(b'\r');s.expect(b'renamed file.txt')
 for cancel in (b'n', b'\r', b'\x1b'):
  s.send(b'd');s.expect(b'Delete file?');s.send(cancel);s.read(.2)
  assert (folder/'destination/renamed file.txt').exists()
 s.send(b'd');s.expect(b'Delete file?');s.send(b'y');s.expect(b'delete complete')
 assert not (folder/'destination/renamed file.txt').exists()
 s.finish()
 # Confirming a non-empty directory deletes nested contents, not symlink targets.
 tree=folder/'delete-tree'
 (tree/'nested/empty').mkdir(parents=True)
 (tree/'nested/child.txt').write_text('Delete only after confirmation\n')
 (tree/'outside-link').symlink_to(folder/'destination', target_is_directory=True)
 (folder/'destination/survivor.txt').write_text('Keep outside target\n')
 s=Session('directory-delete',str(folder))
 s.expect(b'delete-tree')
 s.send(b'/delete-tree');s.expect(b'Filter: delete-tree');s.send(b'\r');s.read(.2)
 for cancel in (b'n', b'\r', b'\x1b'):
  s.send(b'd');s.expect(b'All files and subdirectories');s.send(cancel);s.read(.2)
  assert (tree/'nested/child.txt').exists()
 s.send(b'd');s.expect(b'All files and subdirectories');s.send(b'y');s.expect(b'delete complete')
 assert not tree.exists()
 assert (folder/'destination/survivor.txt').read_text()=='Keep outside target\n'
 s.finish()
 s=Session('editor-failure',str(folder/'keep.txt'),env_override={'VISUAL':'peep-editor-that-does-not-exist'})
 s.expect(b'Do not overwrite');s.send(b'e');s.expect(b'Editor:');s.finish()
 s=Session('help-layout',str(folder))
 s.expect(b'keep.txt');s.resize(55,22);s.read(.2)
 s.send(b'?');s.expect(b'NAVIGATION')
 s.send(b'G');s.expect(b'QUEUE & MOUSE')
 s.send(b'g');s.expect(b'NAVIGATION')
 s.send(b'\x1b');s.expect(b'keep.txt');s.finish()
 print('PASS: overwrite refused, rename/editor/move, file and recursive directory deletion with cancellation, symlink targets preserved')
finally:
 if s is not None and s.proc.poll() is None:s.abort()
