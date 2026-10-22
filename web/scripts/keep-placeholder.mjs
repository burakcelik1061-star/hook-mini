// The build empties internal/webui/dist; put back the placeholder that is
// committed so the Go embed is valid without a build.
import { writeFileSync } from 'node:fs';
writeFileSync(new URL('../../internal/webui/dist/.gitkeep', import.meta.url), '');
