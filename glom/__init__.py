from glom.core import (glom,
                       Fill,
                       Auto,
                       register,
                       register_op,
                       Glommer,
                       Call,
                       Invoke,
                       Spec,
                       Ref,
                       SKIP,
                       STOP,
                       UP,
                       ROOT,
                       MODE,
                       Path,
                       Vars,
                       Val,
                       Coalesce,
                       Inspect,
                       Pipe,
                       GlomError,
                       BadSpec,
                       PathAccessError,
                       PathAssignError,
                       CoalesceError,
                       UnregisteredTarget,
                       T, S, A)

from glom.reduction import Sum, Fold, Flatten, flatten, FoldError, Merge, merge
from glom.matching import (M,
                           Or,
                           And,
                           Not,
                           Match,
                           MatchError,
                           TypeMatchError,
                           Regex,
                           Optional,
                           Required,
                           Switch,
                           Check,
                           CheckError)
from glom.mutation import Assign, Delete, assign, delete, PathDeleteError

# there's no -ion word that really fits what "streaming" means.
# generation, production, iteration, all have more relevant meanings
# elsewhere. (maybe procrastination :P)
from glom.streaming import Iter

from glom._version import __version__

from glom.spec_json import (SPEC_JSON_VERSION,
                            TYPE_REGISTRY,
                            register_spec_type,
                            encode_spec,
                            dumps_spec,
                            dump_spec,
                            load_spec,
                            loads_spec,
                            load_spec_file,
                            render_spec_path,
                            SpecJSONError,
                            SpecEncodeError,
                            SpecDecodeError)
